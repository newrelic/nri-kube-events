// Package events ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package events

import (
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/newrelic/nri-kube-events/pkg/common"
	"github.com/newrelic/nri-kube-events/pkg/router"
)

var (
	requestDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nr",
		Subsystem: "kube_events",
		Name:      "sink_request_duration_seconds",
		Help:      "Duration of requests for each sink",
	}, []string{"sink"})
	eventsReceivedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nr",
		Subsystem: "kube_events",
		Name:      "received_events_total",
		Help:      "Total amount of events received per sink, including failures",
	}, []string{"sink"})
	eventsFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nr",
		Subsystem: "kube_events",
		Name:      "failed_events_total",
		Help:      "Total amount of failed events per sink",
	}, []string{"sink"})
)

type EventHandler interface {
	HandleEvent(kubeEvent common.KubeEvent) error
}

// Router listens for events coming from a SharedIndexInformer,
// and forwards them to the registered sinks
type Router struct {
	// list of handlers to send events to
	handlers map[string]EventHandler

	// all updates & adds will be appended to this queue
	workQueue chan common.KubeEvent

	// all events will be filtered by this list of filters
	excludeFilterProgramms []*vm.Program
}

type observedEventHandler struct {
	EventHandler
	prometheus.Observer
}

func (o *observedEventHandler) HandleEvent(kubeEvent common.KubeEvent) error {
	t := time.Now()
	defer func() { o.Observer.Observe(time.Since(t).Seconds()) }()

	return o.EventHandler.HandleEvent(kubeEvent)
}

// NewRouter returns a new Router which listens to the given SharedIndexInformer,
// and forwards all incoming events to the given sinks
func NewRouter(informer cache.SharedIndexInformer, handlers map[string]EventHandler, filters []string, opts ...router.ConfigOption) *Router {
	config, err := router.NewConfig(opts...)
	if err != nil {
		logrus.Fatalf("Error with Router configuration: %v", err)
	}

	// According to the shared_informer source code it's not designed to
	// wait for the event handlers to finish, they should return quickly
	// Therefore we push to a queue and handle it in another goroutine
	// See: https://github.com/kubernetes/client-go/blob/c8dc69f8a8bf8d8640493ce26688b26c7bfde8e6/tools/cache/shared_informer.go#L111
	workQueue := make(chan common.KubeEvent, config.WorkQueueLength())

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			workQueue <- common.KubeEvent{
				Event: obj.(*v1.Event),
				Verb:  "ADDED",
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			workQueue <- common.KubeEvent{
				Event:    newObj.(*v1.Event),
				OldEvent: oldObj.(*v1.Event),
				Verb:     "UPDATE",
			}
		},
	})

	if err != nil {
		logrus.Warnf("Error with add informer event handlers: %v", err)
	}

	// instrument all sinks with histogram observation
	observedSinks := map[string]EventHandler{}
	for name, handler := range handlers {
		observedSinks[name] = &observedEventHandler{
			EventHandler: handler,
			Observer:     requestDurationSeconds.WithLabelValues(name),
		}
	}

	env := map[string]interface{}{
		"e":    v1.Event{},
		"old":  v1.Event{},
		"verb": "",
	}

	var programms []*vm.Program

	for _, filter := range filters {
		program, err := expr.Compile(filter, expr.Env(env))
		if err != nil {
			logrus.Fatalf("could not compile expression: %v", err)
		}
		programms = append(programms, program)
	}

	return instrument(&Router{
		handlers:               observedSinks,
		excludeFilterProgramms: programms,
		workQueue:              workQueue,
	})
}

func instrument(r *Router) *Router {
	if err := prometheus.Register(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "nr",
			Subsystem: "kube_events",
			Name:      "workqueue_length",
			Help:      "Number of k8s events currently queued in the workqueue.",
		},
		func() float64 {
			return float64(len(r.workQueue))
		},
	)); err != nil {
		logrus.Warningf("could not register workqueue_queue_length prometheus gauge")
	}

	return r
}

// Run listens to the workQueue and forwards incoming events
// to all registered sinks
func (r *Router) Run(stopChan <-chan struct{}) {
	logrus.Infof("Router started")
	defer logrus.Infof("Router stopped")

	for {
		select {
		case <-stopChan:
			return
		case event := <-r.workQueue:
			r.publishEvent(event)
		}
	}
}

func (r *Router) publishEvent(kubeEvent common.KubeEvent) {
	if r.FilterEvent(kubeEvent) {
		logrus.Debugf("Filtered event: %v", kubeEvent)
		return
	}

	for name, handler := range r.handlers {
		eventsReceivedTotal.WithLabelValues(name).Inc()

		if err := handler.HandleEvent(kubeEvent); err != nil {
			logrus.Warningf("Sink %s HandleEvent error: %v", name, err)
			eventsFailuresTotal.WithLabelValues(name).Inc()
		}
	}
}

func (r *Router) FilterEvent(event common.KubeEvent) bool {
	env := map[string]interface{}{
		"e":    event.Event,
		"old":  event.OldEvent,
		"verb": event.Verb,
	}
	for _, program := range r.excludeFilterProgramms {
		output, err := expr.Run(program, env)
		if err != nil {
			logrus.Fatalf("could not run expression: %v", err)
		}

		if output.(bool) {
			return true
		}
	}
	return false
}
