// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/newrelic/nri-kube-events/pkg/descriptions"
	"github.com/newrelic/nri-kube-events/pkg/events"
	"github.com/newrelic/nri-kube-events/pkg/router"
	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

var (
	integrationVersion = "0.0.0"
	gitCommit          = ""
	buildDate          = ""
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

	// load custom resource filters
	rawFilters := os.Getenv("CR_FILTERS")
	crFilters := []string{}
	if rawFilters != "" {
		err := json.Unmarshal([]byte(rawFilters), &crFilters)
		if err != nil {
			fmt.Printf("Failed to monitor custom resources. Error parsing customResourceFilters: %v.\n", err)
		}
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

	if cfg.CaptureDescribe == nil || *cfg.CaptureDescribe {
		resync := DefaultDescribeRefresh
		if cfg.DescribeRefresh != nil {
			resync = *cfg.DescribeRefresh
		}
		resourceInformers := createInformers(crFilters, stopChan, resync)
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

	go func() {
		err := server.ListenAndServe()
		logrus.Fatalf("Could not serve Prometheus on %s: %v", addr, err)
	}()

	<-stopChan
	err := server.Shutdown(context.Background())
	logrus.WithError(err).Warn("Failed to gracefully shutdown prometheus server")
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
func createInformers(crFilters []string, stopChan <-chan struct{}, resync time.Duration) []cache.SharedIndexInformer {
	crFilterMatchers := make([]*regexp.Regexp, len(crFilters))
	for i, filter := range crFilters {
		matcher, err := regexp.Compile(filter)
		if err != nil {
			logrus.Fatalf("failed to compile regex from customResourceFilters: %v", err)
		}
		crFilterMatchers[i] = matcher
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		logrus.Fatalf("failed to get pod service account config: %v", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		logrus.Fatalf("could not create discovery client: %v", err)
	}

	_, resourceMap, _, err := discoveryClient.GroupsAndMaybeResources()
	if err != nil {
		logrus.Fatalf("could not discover groups and/or resources: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		logrus.Fatalf("could not create dynamic client: %v", err)
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, resync, corev1.NamespaceAll, nil)

	var informers []cache.SharedIndexInformer
	for gv, list := range resourceMap {
		for _, resource := range list.APIResources {
			if !shouldWatchResource(resource) {
				continue
			}

			gvk := gv.WithKind(resource.Kind)
			gvr := gv.WithResource(resource.Name)

			shouldMonitor := false
			if scheme.Scheme.Recognizes(gvk) {
				// always monitor built-in resources
				shouldMonitor = true
			} else {
				// monitor a CR if it matches any of the filters from config
				gvrKey := fmt.Sprintf("%s/%s/%s", gvr.Group, gvr.Version, gvr.Resource)
				for _, m := range crFilterMatchers {
					if m.MatchString(gvrKey) {
						shouldMonitor = true
						break
					}
				}
			}

			if shouldMonitor {
				resourceInformer := factory.ForResource(gvr).Informer()
				informers = append(informers, resourceInformer)
			}
		}
	}

	factory.Start(stopChan)

	return informers
}

func shouldWatchResource(ar metav1.APIResource) bool {
	isTopLevelResource := !strings.Contains(ar.Name, "/")
	isWatchable := slices.Contains(ar.Verbs, "watch")
	return isTopLevelResource && isWatchable
}

// getClientset returns a kubernetes clientset.
// It loads a kubeconfig file if the kubeconfig parameter is set
// If it's not set, it will try to load the InClusterConfig
func getClientset(kubeconfig string) (*kubernetes.Clientset, error) {
	var conf *rest.Config
	var err error

	if kubeconfig != "" {
		conf, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		conf, err = rest.InClusterConfig()
	}

	if err != nil {
		return nil, fmt.Errorf("cannot load kubernetes client configuration: %w", err)
	}

	return kubernetes.NewForConfig(conf)
}

func setLogLevel(logLevel string, fallback logrus.Level) {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Warningf("invalid loglevel %s, defaulting to %s.", logLevel, fallback.String())
		level = fallback
	}

	logrus.SetLevel(level)
}
