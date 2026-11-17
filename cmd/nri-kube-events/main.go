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

	if cfg.ObjectState.Enabled {
		startObjectState(cfg.ObjectState, stopChan, wg)
	}

	if cfg.CaptureDescribe == nil || *cfg.CaptureDescribe {
		resync := DefaultDescribeRefresh
		if cfg.DescribeRefresh != nil {
			resync = *cfg.DescribeRefresh
		}
		resourceInformers := createInformers(stopChan, resync)
		activeObjectHandlers := make(map[string]descriptions.ObjectHandler)

		for name, sink := range activeSinks {
			activeObjectHandlers[name] = sink
		}

		descRouter := descriptions.NewRouter(resourceInformers, activeObjectHandlers, opts...)

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

// startObjectState validates oc, loads the HMAC key and the hash store, and starts the
// object-state carrier, which runs until stopChan closes. It also starts the command API
// poller and runs the watcher only while the account flag is on. Any startup failure is
// fatal.
func startObjectState(oc ObjectStateConfig, stopChan <-chan struct{}, wg *sync.WaitGroup) {
	if oc.ClusterName == "" {
		logrus.Fatal("objectState.enabled is true but objectState.clusterName is empty")
	}
	if oc.AgentEndpoint == "" {
		logrus.Fatal("objectState.enabled is true but objectState.agentEndpoint is empty")
	}
	if oc.Namespace == "" {
		logrus.Fatal("objectState.enabled is true but objectState.namespace is empty")
	}
	licenseKey := os.Getenv(licenseKeyEnv)
	if licenseKey == "" {
		logrus.Fatalf("objectState.enabled is true but the %s env var is empty", licenseKeyEnv)
	}

	conf, err := getRestConfig(*kubeConfig)
	if err != nil {
		logrus.Fatalf("objects: %v", err)
	}
	// The carrier lists every kind at startup, which at the default QPS of 5 takes about
	// 45 s on a cluster with 233 kinds. The higher limits apply to this copy only. The copy
	// keeps the base config's zero Timeout, because a client timeout would end every watch.
	conf = restclient.CopyConfig(conf)
	conf.QPS = objectStateQPS
	conf.Burst = objectStateBurst

	clientset, err := kubernetes.NewForConfig(conf)
	if err != nil {
		logrus.Fatalf("objects: could not create kubernetes client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(conf)
	if err != nil {
		logrus.Fatalf("objects: could not create dynamic client: %v", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(conf)
	if err != nil {
		logrus.Fatalf("objects: could not create discovery client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stopChan
		cancel()
	}()

	startCtx, startCancel := context.WithTimeout(ctx, objectStateStartupTimeout)
	defer startCancel()
	hmacKey, err := objects.LoadOrCreateHMACKey(startCtx, clientset, oc.Namespace)
	if err != nil {
		logrus.Fatalf("objects: could not load or create the HMAC key secret in namespace %s: %v", oc.Namespace, err)
	}
	// This is the only load of the hash store: Run does not load it again, so a transient
	// apiserver error after startup cannot stop the carrier.
	store := objects.NewStore(clientset, oc.Namespace)
	if err = store.Load(startCtx); err != nil {
		logrus.Fatalf("objects: could not load the hash store in namespace %s: %v", oc.Namespace, err)
	}

	if err = objects.RegisterMetrics(prometheus.DefaultRegisterer); err != nil {
		logrus.Fatalf("objects: could not register metrics: %v", err)
	}
	if err = accountflag.RegisterMetrics(prometheus.DefaultRegisterer); err != nil {
		logrus.Fatalf("objects: could not register account flag metrics: %v", err)
	}

	sink, err := objects.NewInfraSink(objects.InfraSinkConfig{
		Endpoint:            oc.AgentEndpoint,
		ClusterName:         oc.ClusterName,
		IntegrationVersion:  integrationVersion,
		MaxRecordsPerSecond: oc.maxRecordsPerSecond(),
	})
	if err != nil {
		logrus.Fatalf("objects: %v", err)
	}

	// With no HTTPClient the poller builds its own client, with a request timeout, through
	// Proxy.
	poller, err := accountflag.NewPoller(accountflag.Config{
		URL:        accountflag.CommandAPIURL(licenseKey, oc.NRStaging, oc.Fedramp),
		LicenseKey: licenseKey,
		UserAgent:  "nri-kube-events/" + integrationVersion,
		Proxy:      oc.Proxy,
	})
	if err != nil {
		logrus.Fatalf("objects: %v", err)
	}
	flagState := func() string { return poller.State().String() }

	watcherConfig := objects.Config{
		ClusterName:       oc.ClusterName,
		Namespace:         oc.Namespace,
		ResendAfter:       oc.resendAfter(),
		MaxResendsPerScan: maxResendsPerScan(oc.maxRecordsPerSecond()),
		AccountFlag:       flagState,
	}
	// A Watcher runs once, so every run builds a new one over the same store.
	run := func(ctx context.Context) error {
		return objects.NewWatcher(watcherConfig, dynamicClient, discoveryClient, store, sink, hmacKey).Run(ctx)
	}

	logrus.Infof("objects: starting the object-state carrier for cluster %s, hash store in namespace %s", oc.ClusterName, oc.Namespace)
	wg.Add(1)
	go func() {
		defer wg.Done()
		poller.Run(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		watcherExited(ctx, objects.RunGated(ctx, poller, run))
	}()
}

// maxResendsPerScan caps one resend scan at the records the sink posts in one resend
// interval, and at half the client QPS over that interval, since each resend first GETs its
// object. The other half stays free for the reflectors' lists and the retries.
func maxResendsPerScan(maxRecordsPerSecond float64) int {
	perSecond := min(maxRecordsPerSecond, objectStateQPS/2)
	return int(perSecond * objects.DefaultResendInterval.Seconds())
}

// watcherExited handles the return of the carrier's run. The run returns only after ctx
// is canceled, so a return while ctx is live means the carrier stopped while the events
// path keeps running. The process then exits, so the pod restarts. A return after the stop
// signal is the normal shutdown, and its flush error is only logged.
func watcherExited(ctx context.Context, err error) {
	if ctx.Err() == nil {
		logrus.Fatalf("objects: watcher stopped before the stop signal: %v", err)
		return
	}
	if err != nil {
		logrus.Errorf("objects: watcher stopped: %v", err)
	}
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
func createInformers(stopChan <-chan struct{}, resync time.Duration) []cache.SharedIndexInformer {
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

	return []cache.SharedIndexInformer{
		cronjobsInformer,
		daemonsetsInformer,
		deploymentInformer,
		namespacesInformer,
		nodesInformer,
		jobsInformer,
		pvInformer,
		pvcInformer,
		podsInformer,
		servicesInformer,
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
