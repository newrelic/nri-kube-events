// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/newrelic/nri-kube-events/pkg/accountflag"
	"github.com/newrelic/nri-kube-events/pkg/descriptions"
	"github.com/newrelic/nri-kube-events/pkg/events"
	"github.com/newrelic/nri-kube-events/pkg/objects"
	"github.com/newrelic/nri-kube-events/pkg/router"
	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

var (
	integrationVersion = "0.0.0"
	gitCommit          = ""
	buildDate          = ""
)

const (
	licenseKeyEnv = "NEW_RELIC_LICENSE_KEY"

	objectStateQPS            = 50
	objectStateBurst          = 100
	objectStateStartupTimeout = 2 * time.Minute
	// objectStateResendQPS is the share of objectStateQPS the resend's GETs may use.
	objectStateResendQPS = objectStateQPS / 2
	// objectStateRetryFactor multiplies the wait after each failure of the carrier.
	objectStateRetryFactor = 2
)

var (
	configFile = flag.String("config", "config.yaml", "location of the configuration file")
	kubeConfig = flag.String("kubeconfig", "", "location of the k8s configuration file. Usually in ~/.kube/config")
	logLevel   = flag.String("loglevel", "info", "Log level: [warning, info, debug]")
	promAddr   = flag.String("promaddr", "0.0.0.0:8080", "Address to serve prometheus metrics on")
)

func main() {
	flag.Parse()
	setLogLevel(*logLevel, logrus.InfoLevel)

	logrus.Infof(
		"New Relic Kube Events integration Version: %s, Platform: %s, GoVersion: %s, GitCommit: %s, BuildDate: %s",
		integrationVersion,
		fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		runtime.Version(),
		gitCommit,
		buildDate)
	cfg := mustLoadConfigFile(*configFile)

	activeSinks, err := sinks.Create(cfg.Sinks, integrationVersion)
	if err != nil {
		logrus.Fatalf("could not create sinks: %v", err)
	}

	wg := &sync.WaitGroup{}
	stopChan := listenForStopSignal()

	opts := []router.ConfigOption{
		router.WithWorkQueueLength(cfg.WorkQueueLength), // will ignore null values
	}

	if cfg.CaptureEvents == nil || *cfg.CaptureEvents {
		eventsInformer := createEventsInformer(stopChan)
		activeEventHandlers := make(map[string]events.EventHandler)

		for name, sink := range activeSinks {
			activeEventHandlers[name] = sink
		}

		eventRouter := events.NewRouter(eventsInformer, activeEventHandlers, opts...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			eventRouter.Run(stopChan)
		}()
	}

	captureDescribe := cfg.CaptureDescribe == nil || *cfg.CaptureDescribe
	coverage := describeCoverage(captureDescribe, cfg.ObjectState)
	if captureDescribe {
		resync := DefaultDescribeRefresh
		if cfg.DescribeRefresh != nil {
			resync = *cfg.DescribeRefresh
		}
		resourceInformers := createInformers(stopChan, resync)
		activeObjectHandlers := make(map[string]descriptions.ObjectHandler)

		for name, sink := range activeSinks {
			activeObjectHandlers[name] = sink
		}

		// A nil *objects.Coverage must reach NewRouter as a nil interface.
		var routerCoverage descriptions.Coverage
		if coverage != nil {
			routerCoverage = coverage
			logrus.Info("Descriptions skip the resources that object state collects while it collects them")
		}
		descRouter := descriptions.NewRouter(resourceInformers, activeObjectHandlers, routerCoverage, opts...)

		wg.Add(1)
		go func() {
			defer wg.Done()
			descRouter.Run(stopChan)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		servePrometheus(*promAddr, stopChan)
	}()

	// The carrier starts after the Kubernetes Events and describe paths, and never blocks
	// or stops them.
	// With the carrier off nothing starts and no objects metric is registered.
	if cfg.ObjectState.Enabled {
		startObjectState(cfg.ObjectState, coverage, stopChan, wg)
	}

	// After the stop signal, main waits for every path to return; upstream exited with
	// status 1 as soon as the Prometheus server shut down. The wait lets the carrier flush
	// its hash store. It applies with the carrier off too: shutdown then waits for the
	// events and describe routers, so a sink call blocked in its retries delays the exit
	// until the pod's termination grace period ends.
	wg.Wait()
	logrus.Infoln("Shutdown complete")
}

func servePrometheus(addr string, stopChan <-chan struct{}) {
	logrus.Infof("Serving Prometheus metrics on %s", addr)

	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 3 * time.Second,
		Handler:           promhttp.Handler(),
	}

	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := serveError(addr, server.ListenAndServe()); err != nil {
			logrus.Fatal(err)
		}
		logrus.Info("Prometheus server closed")
	}()

	<-stopChan
	if err := server.Shutdown(context.Background()); err != nil {
		logrus.WithError(err).Warn("Failed to gracefully shutdown prometheus server")
	}
	<-served
}

// serveError returns nil when ListenAndServe returned because Shutdown closed the server,
// which is a normal stop, and an error for every other return.
func serveError(addr string, err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("could not serve Prometheus on %s: %w", addr, err)
}

// objectStateRetry is the wait after a failed start or run of the object-state carrier. It
// doubles after each failure up to max, and starts over after a run that lasted longer than
// max.
var objectStateRetry = retryPolicy{initial: 5 * time.Second, max: 5 * time.Minute}

// objectStateRegisterer is where the objects metrics register. Tests replace it.
var objectStateRegisterer prometheus.Registerer = prometheus.DefaultRegisterer

type retryPolicy struct {
	initial, max time.Duration
}

// describeCoverage returns the Coverage the carrier writes and the describe path reads, or
// nil when the describe path describes every object: when describe capture is off or the
// carrier is off. The describe path skips a resource only while the running carrier covers
// it, so with the account flag off, or with a carrier that failed to start, describe
// capture is unchanged.
func describeCoverage(captureDescribe bool, oc ObjectStateConfig) *objects.Coverage {
	if !captureDescribe || !oc.Enabled {
		return nil
	}
	return objects.NewCoverage()
}

// startObjectState starts the object-state carrier in its own goroutine and returns at once.
// The carrier runs until stopChan closes. No failure of the carrier stops the process, so
// Kubernetes Events and describe collection keep running: an invalid oc is logged and the
// carrier does not start, and a failed startup or a run that ends early is logged, counted
// in nri_kube_events_objects_carrier_failures_total, and retried with backoff. The carrier
// polls the command API and runs the watcher only while the account flag is on.
func startObjectState(oc ObjectStateConfig, coverage *objects.Coverage, stopChan <-chan struct{}, wg *sync.WaitGroup) {
	if err := objects.RegisterMetrics(objectStateRegisterer); err != nil {
		logrus.WithError(err).Error("objects: could not register metrics")
	}
	if err := oc.validate(); err != nil {
		objects.CountCarrierFailure(objects.StageConfig)
		logrus.WithError(err).Error("objects: not starting the object-state carrier; Kubernetes Events collection continues")
		return
	}
	if err := accountflag.RegisterMetrics(objectStateRegisterer); err != nil {
		logrus.WithError(err).Error("objects: could not register account flag metrics")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stopChan
		cancel()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		var c *objectCarrier
		retryObjectState(ctx, objectStateRetry, func(ctx context.Context) (string, error) {
			if c == nil {
				started, err := newObjectCarrier(ctx, oc, coverage)
				if err != nil {
					return objects.StageStartup, err
				}
				c = started
				logrus.Infof("objects: starting the object-state carrier for cluster %s, hash store in namespace %s", oc.ClusterName, oc.Namespace)
			}
			return objects.StageRun, c.run(ctx)
		})
	}()
}

// retryObjectState calls attempt until ctx is canceled. attempt returns only on failure, or
// after ctx is canceled, and names the stage it failed at. Each failure while ctx is live is
// logged and counted, and the next attempt waits per p. An error returned after the cancel
// is the shutdown flush's, and is only logged.
func retryObjectState(ctx context.Context, p retryPolicy, attempt func(context.Context) (string, error)) {
	delay := p.initial
	for {
		began := time.Now()
		stage, err := attempt(ctx)
		if ctx.Err() != nil {
			if err != nil {
				logrus.WithError(err).Error("objects: object-state carrier stopped")
			}
			return
		}
		if err == nil {
			err = errors.New("returned before the stop signal")
		}
		if time.Since(began) > p.max {
			delay = p.initial
		}
		objects.CountCarrierFailure(stage)
		logrus.WithError(err).WithField("stage", stage).Errorf("objects: object-state carrier failed, retrying in %s; Kubernetes Events collection continues", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(objectStateRetryFactor*delay, p.max)
	}
}

// objectCarrier is a started object-state carrier: its clients, HMAC key, loaded hash store,
// sink and its command API poller.
type objectCarrier struct {
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	store           *objects.Store
	sink            *objects.InfraSink
	poller          *accountflag.Poller
	hmacKey         []byte
	watcherConfig   objects.Config
}

// newObjectCarrier builds the carrier's clients, loads the HMAC key and the hash store
// within objectStateStartupTimeout, and builds the sink and the poller. oc is valid.
func newObjectCarrier(ctx context.Context, oc ObjectStateConfig, coverage *objects.Coverage) (*objectCarrier, error) {
	licenseKey := os.Getenv(licenseKeyEnv)
	if licenseKey == "" {
		return nil, fmt.Errorf("objectState.enabled is true but the %s env var is empty", licenseKeyEnv)
	}

	conf, err := getRestConfig(*kubeConfig)
	if err != nil {
		return nil, err
	}
	// The carrier lists every kind at startup, which at the default QPS of 5 takes about
	// 45 s on a cluster with 233 kinds. The higher limits apply to this copy only. The copy
	// keeps the base config's zero Timeout, because a client timeout would end every watch.
	conf = restclient.CopyConfig(conf)
	conf.QPS = objectStateQPS
	conf.Burst = objectStateBurst

	clientset, err := kubernetes.NewForConfig(conf)
	if err != nil {
		return nil, fmt.Errorf("could not create kubernetes client: %w", err)
	}
	c := &objectCarrier{}
	if c.dynamicClient, err = dynamic.NewForConfig(conf); err != nil {
		return nil, fmt.Errorf("could not create dynamic client: %w", err)
	}
	if c.discoveryClient, err = discovery.NewDiscoveryClientForConfig(conf); err != nil {
		return nil, fmt.Errorf("could not create discovery client: %w", err)
	}

	startCtx, startCancel := context.WithTimeout(ctx, objectStateStartupTimeout)
	defer startCancel()
	if c.hmacKey, err = objects.LoadOrCreateHMACKey(startCtx, clientset, oc.Namespace); err != nil {
		return nil, fmt.Errorf("could not load or create the HMAC key secret in namespace %s: %w", oc.Namespace, err)
	}
	// This is the only load of the hash store: Run does not load it again, and a run that
	// is restarted reuses this store.
	c.store = objects.NewStore(clientset, oc.Namespace)
	if err = c.store.Load(startCtx); err != nil {
		return nil, fmt.Errorf("could not load the hash store in namespace %s: %w", oc.Namespace, err)
	}

	c.sink, err = objects.NewInfraSink(objects.InfraSinkConfig{
		Endpoint:            oc.AgentEndpoint,
		ClusterName:         oc.ClusterName,
		IntegrationVersion:  integrationVersion,
		MaxRecordsPerSecond: oc.maxRecordsPerSecond(),
	})
	if err != nil {
		return nil, err
	}

	// With no HTTPClient the poller builds its own client, with a request timeout, through
	// Proxy.
	c.poller, err = accountflag.NewPoller(accountflag.Config{
		URL:        accountflag.CommandAPIURL(licenseKey, oc.NRStaging, oc.Fedramp),
		LicenseKey: licenseKey,
		UserAgent:  "nri-kube-events/" + integrationVersion,
		Proxy:      oc.Proxy,
	})
	if err != nil {
		return nil, err
	}
	poller := c.poller
	flagState := func() string { return poller.State().String() }
	c.watcherConfig = objects.Config{
		ClusterName:       oc.ClusterName,
		Namespace:         oc.Namespace,
		ResendAfter:       oc.resendAfter(),
		MaxResendsPerScan: maxResendsPerScan(oc.maxRecordsPerSecond()),
		AccountFlag:       flagState,
		Coverage:          coverage,
	}
	return c, nil
}

// run runs the carrier until ctx is canceled, and returns early only if the watcher does.
// It runs the watcher only while the account flag is on.
func (c *objectCarrier) run(ctx context.Context) error {
	// A Watcher runs once, so every run builds a new one over the same store.
	runWatcher := func(ctx context.Context) error {
		return objects.NewWatcher(c.watcherConfig, c.dynamicClient, c.discoveryClient, c.store, c.sink, c.hmacKey).Run(ctx)
	}
	pollCtx, stopPoll := context.WithCancel(ctx)
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		c.poller.Run(pollCtx)
	}()
	err := objects.RunGated(ctx, c.poller, runWatcher)
	stopPoll()
	<-polled
	return err
}

// maxResendsPerScan caps one resend scan at the records the sink posts in one resend
// interval, and at half the client QPS over that interval, since each resend first GETs its
// object. The other half stays free for the reflectors' lists and the retries.
func maxResendsPerScan(maxRecordsPerSecond float64) int {
	perSecond := min(maxRecordsPerSecond, objectStateResendQPS)
	return int(perSecond * objects.DefaultResendInterval.Seconds())
}

// listenForStopSignal returns a channel that will be closed
// when a SIGINT or SIGTERM signal is received
func listenForStopSignal() <-chan struct{} {
	stopChan := make(chan struct{})
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		sig := <-c

		logrus.Infof("%s signal detected, stopping server.", sig)
		close(stopChan)
	}()

	return stopChan
}

// createEventsInformer creates a SharedIndexInformer that will listen for Events.
// Only events happening after creation will be returned, existing events are discarded.
func createEventsInformer(stopChan <-chan struct{}) cache.SharedIndexInformer {
	clientset, err := getClientset(*kubeConfig)
	if err != nil {
		logrus.Fatalf("could not create kubernetes client: %v", err)
	}

	// Setting resync to 0 means the SharedInformer will never refresh its internal cache against the API Server.
	// This is important, because later on we clear the initial cache.
	resync := time.Duration(0)
	sharedInformers := informers.NewSharedInformerFactory(clientset, resync)
	eventsInformer := sharedInformers.Core().V1().Events().Informer()

	sharedInformers.Start(stopChan)

	// wait for the internal cache to sync. This is the only time the cache will be filled,
	// since we've set resync to 0. This behavior is very important,
	// because we will delete the cache to prevent duplicate events from being sent.
	// If we remove this cache-deletion and you restart nri-kube-events, we will sent lots of duplicated events
	sharedInformers.WaitForCacheSync(stopChan)

	// There doesn't seem to be a way to start a SharedInformer without local cache,
	// So we manually delete the cached events. We are only interested in new events.
	for _, obj := range eventsInformer.GetStore().List() {
		if err := eventsInformer.GetStore().Delete(obj); err != nil {
			logrus.Warningln("Unable to delete cached event, duplicated event is possible")
		}
	}

	return eventsInformer
}

// createInformers creates a SharedIndexInformer that will listen for resources we care aobut.
func createInformers(stopChan <-chan struct{}, resync time.Duration) []descriptions.Informer {
	clientset, err := getClientset(*kubeConfig)
	if err != nil {
		logrus.Fatalf("could not create kubernetes client: %v", err)
	}

	sharedInformers := informers.NewSharedInformerFactory(clientset, resync)

	cronjobsInformer := sharedInformers.Batch().V1().CronJobs().Informer()
	daemonsetsInformer := sharedInformers.Apps().V1().DaemonSets().Informer()
	deploymentInformer := sharedInformers.Apps().V1().Deployments().Informer()
	namespacesInformer := sharedInformers.Core().V1().Namespaces().Informer()
	nodesInformer := sharedInformers.Core().V1().Nodes().Informer()
	jobsInformer := sharedInformers.Batch().V1().Jobs().Informer()
	pvInformer := sharedInformers.Core().V1().PersistentVolumes().Informer()
	pvcInformer := sharedInformers.Core().V1().PersistentVolumeClaims().Informer()
	podsInformer := sharedInformers.Core().V1().Pods().Informer()
	servicesInformer := sharedInformers.Core().V1().Services().Informer()

	sharedInformers.Start(stopChan)

	return []descriptions.Informer{
		{SharedIndexInformer: cronjobsInformer, Resource: schema.GroupResource{Group: "batch", Resource: "cronjobs"}},
		{SharedIndexInformer: daemonsetsInformer, Resource: schema.GroupResource{Group: "apps", Resource: "daemonsets"}},
		{SharedIndexInformer: deploymentInformer, Resource: schema.GroupResource{Group: "apps", Resource: "deployments"}},
		{SharedIndexInformer: namespacesInformer, Resource: schema.GroupResource{Resource: "namespaces"}},
		{SharedIndexInformer: nodesInformer, Resource: schema.GroupResource{Resource: "nodes"}},
		{SharedIndexInformer: jobsInformer, Resource: schema.GroupResource{Group: "batch", Resource: "jobs"}},
		{SharedIndexInformer: pvInformer, Resource: schema.GroupResource{Resource: "persistentvolumes"}},
		{SharedIndexInformer: pvcInformer, Resource: schema.GroupResource{Resource: "persistentvolumeclaims"}},
		{SharedIndexInformer: podsInformer, Resource: schema.GroupResource{Resource: "pods"}},
		{SharedIndexInformer: servicesInformer, Resource: schema.GroupResource{Resource: "services"}},
	}
}

// getClientset returns a kubernetes clientset.
// It loads a kubeconfig file if the kubeconfig parameter is set
// If it's not set, it will try to load the InClusterConfig
func getClientset(kubeconfig string) (*kubernetes.Clientset, error) {
	conf, err := getRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(conf)
}

// getRestConfig loads the kubeconfig file if kubeconfig is set, else the InClusterConfig.
func getRestConfig(kubeconfig string) (*restclient.Config, error) {
	var conf *restclient.Config
	var err error

	if kubeconfig != "" {
		conf, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		conf, err = restclient.InClusterConfig()
	}

	if err != nil {
		return nil, fmt.Errorf("cannot load kubernetes client configuration: %w", err)
	}

	return conf, nil
}

func setLogLevel(logLevel string, fallback logrus.Level) {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Warningf("invalid loglevel %s, defaulting to %s.", logLevel, fallback.String())
		level = fallback
	}

	logrus.SetLevel(level)
}
