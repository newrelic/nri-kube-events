// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/tools/cache"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/newrelic/nri-kube-events/pkg/events"
	"github.com/newrelic/nri-kube-events/pkg/sinks"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

const (
	integrationName = "nri-kube-events"
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
		"New Relic %s integration Version: %s, Platform: %s, GoVersion: %s, GitCommit: %s, BuildDate: %s\n",
		strings.Title(strings.Replace(integrationName, "com.newrelic.", "", 1)),
		integrationVersion,
		fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		runtime.Version(),
		gitCommit,
		buildDate)
	cfg := mustLoadConfigFile(*configFile)

	activeSinks, err := sinks.CreateSinks(cfg.Sinks, integrationVersion)
	if err != nil {
		logrus.Fatalf("could not create sinks: %v", err)
	}

	wg := &sync.WaitGroup{}
	stopChan := listenForStopSignal()

	eventsInformer, err := createEventsInformer(stopChan)
	if err != nil {
		logrus.Fatalf("could not create EventsInformer: %v", err)
	}

	opts := []events.RouterConfigOption{
		events.WithWorkQueueLength(cfg.WorkQueueLength), // will ignore null values
	}

	router := events.NewRouter(eventsInformer, activeSinks, opts...)

	wg.Add(1)
	go func() {
		defer wg.Done()
		router.Run(stopChan)
	}()

	go servePrometheus(*promAddr)

	wg.Wait()
	logrus.Infoln("Shutdown complete")
}

func servePrometheus(addr string) {
	logrus.Infof("Serving Prometheus metrics on %s\n", addr)
	err := http.ListenAndServe(addr, promhttp.Handler())
	logrus.Fatalf("Could not serve Prometheus on %s: %v", addr, err)
}

// listenForStopSignal returns a channel that will be closed
// when a SIGINT or SIGTERM signal is received
func listenForStopSignal() <-chan struct{} {
	stopChan := make(chan struct{})
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		sig := <-c

		logrus.Infof("%s signal detected, stopping server.\n", sig)
		close(stopChan)
	}()

	return stopChan
}

// createEventsInformer creates a SharedIndexInformer that will listen for Events.
// Only events happening after creation will be returned, existing events are discarded.
func createEventsInformer(stopChan <-chan struct{}) (cache.SharedIndexInformer, error) {
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
	// since we've set resync to 0. This behaviour is very important,
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

	return eventsInformer, nil
}

// getClientset returns a kubernetes clientset.
// It loads a kubeconfig file if the kubeconfig parameter is set
// If it's not set, it will try to load the InClusterConfig
func getClientset(kubeconfig string) (*kubernetes.Clientset, error) {
	var conf *restclient.Config
	var err error

	if kubeconfig != "" {
		conf, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		conf, err = restclient.InClusterConfig()
	}

	if err != nil {
		return nil, errors.Wrap(err, "cannot load kubernetes client configuration")
	}

	return kubernetes.NewForConfig(conf)
}

func setLogLevel(logLevel string, fallback logrus.Level) {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Warningf("invalid loglevel %s, defaulting to %s.\n", logLevel, fallback.String())
		level = fallback
	}

	logrus.SetLevel(level)
}
