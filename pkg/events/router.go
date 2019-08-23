// Package events ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package events

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
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

// KubeEvent represents a Kubernetes event. It specifies if this is the first
// time the event is seen or if it's an update to a previous event.
type KubeEvent struct {
	Verb     string    `json:"verb"`
	Event    *v1.Event `json:"event"`
	OldEvent *v1.Event `json:"old_event,omitempty"`
}

// Router listens for events coming from a SharedIndexInformer,
// and forwards them to the registered sinks
type Router struct {
	// list of sinks to send events to
	sinks map[string]Sink

	// all updates & adds will be appended to this queue
	workQueue chan KubeEvent
}

// NewRouter returns a new Router which listens to the given SharedIndexInformer,
// and forwards all incoming events to the given sinks
func NewRouter(informer cache.SharedIndexInformer, sinks map[string]Sink, opts ...RouterConfigOption) *Router {

	// default config values for our Router
	config := routerConfig{
		workQueueLength: 1024,
	}

	for _, opt := range opts {
		if err := opt(&config); err != nil {
			logrus.Fatalf("Error with Router configuration: %v", err)
		}
	}

	// According to the shared_informer source code it's not designed to
	// wait for the event handlers to finish, they should return quickly
	// Therefor we push to a queue and handle it in another goroutine
	// See: https://github.com/kubernetes/client-go/blob/c8dc69f8a8bf8d8640493ce26688b26c7bfde8e6/tools/cache/shared_informer.go#L111
	workQueue := make(chan KubeEvent, config.workQueueLength)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			workQueue <- KubeEvent{
				Event: obj.(*v1.Event),
				Verb:  "ADDED",
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			workQueue <- KubeEvent{
				Event:    newObj.(*v1.Event),
				OldEvent: oldObj.(*v1.Event),
				Verb:     "UPDATE",
			}
		},
	})
	// instrument all sinks with histogram observation
	observedSinks := map[string]Sink{}
	for name, sink := range sinks {
		observedSinks[name] = &observedSink{
			sink:     sink,
			observer: requestDurationSeconds.WithLabelValues(name),
		}
	}

	return instrument(&Router{
		sinks:     observedSinks,
		workQueue: workQueue,
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

func (r *Router) publishEvent(kubeEvent KubeEvent) {
	for name, sink := range r.sinks {

		eventsReceivedTotal.WithLabelValues(name).Inc()

		if err := sink.HandleEvent(kubeEvent); err != nil {
			logrus.Warningf("Sink %s HandleEvent error: %v", name, err)
			eventsFailuresTotal.WithLabelValues(name).Inc()
		}
	}
}
