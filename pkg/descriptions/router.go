// Package descriptions ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package descriptions

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/newrelic/nri-kube-events/pkg/router"
)

var (
	requestDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nr",
		Subsystem: "k8s_descriptions",
		Name:      "sink_request_duration_seconds",
		Help:      "Duration of requests for each sink",
	}, []string{"sink"})
	descsReceivedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nr",
		Subsystem: "k8s_descriptions",
		Name:      "received",
		Help:      "Total amount of descriptions received per sink, including failures",
	}, []string{"sink"})
	descsFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nr",
		Subsystem: "k8s_descriptions",
		Name:      "failed",
		Help:      "Total amount of failed descriptions per sink",
	}, []string{"sink"})
)

type ObjectHandler interface {
	HandleObject(kubeEvent runtime.Object) error
}

// Router listens for events coming from a SharedIndexInformer,
// and forwards them to the registered sinks
type Router struct {
	// list of handlers to send events to
	handlers map[string]ObjectHandler

	// all updates & adds will be appended to this queue
	workQueue chan runtime.Object
}

type observedObjectHandler struct {
	ObjectHandler
	prometheus.Observer
}

func (o *observedObjectHandler) HandleObject(obj runtime.Object) error {
	t := time.Now()
	defer func() { o.Observer.Observe(time.Since(t).Seconds()) }()

	return o.ObjectHandler.HandleObject(obj)
}

// NewRouter returns a new Router which listens to the given SharedIndexInformer,
// and forwards all incoming events to the given sinks
func NewRouter(informers []cache.SharedIndexInformer, handlers map[string]ObjectHandler, opts ...router.ConfigOption) *Router {
	config, err := router.NewConfig(opts...)
	if err != nil {
		logrus.Fatalf("Error with Router configuration: %v", err)
	}

	workQueue := make(chan runtime.Object, config.WorkQueueLength())

	for _, informer := range informers {
		_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				workQueue <- obj.(runtime.Object)
			},
			UpdateFunc: func(_, newObj interface{}) {
				workQueue <- newObj.(runtime.Object)
			},
		})

		if err != nil {
			logrus.Warnf("Error with add informer event handlers: %v", err)
		}
	}

	// instrument all sinks with histogram observation
	observedSinks := map[string]ObjectHandler{}
	for name, handler := range handlers {
		observedSinks[name] = &observedObjectHandler{
			ObjectHandler: handler,
			Observer:      requestDurationSeconds.WithLabelValues(name),
		}
	}

	return instrument(&Router{
		handlers:  observedSinks,
		workQueue: workQueue,
	})
}

func instrument(r *Router) *Router {
	if err := prometheus.Register(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "nr",
			Subsystem: "k8s_descriptions",
			Name:      "workqueue_length",
			Help:      "Number of k8s objects currently queued in the workqueue.",
		},
		func() float64 {
			return float64(len(r.workQueue))
		},
	)); err != nil {
		logrus.Warningf("could not register workqueue_queue_length prometheus gauge")
	}

	return r
}

// Run listens to the workQueue and forwards incoming objects
// to all registered sinks
func (r *Router) Run(stopChan <-chan struct{}) {
	logrus.Infof("Router started")
	defer logrus.Infof("Router stopped")

	for {
		select {
		case <-stopChan:
			return
		case obj := <-r.workQueue:
			r.publishObjectDescription(obj)
		}
	}
}

func (r *Router) publishObjectDescription(obj runtime.Object) {
	for name, handler := range r.handlers {
		descsReceivedTotal.WithLabelValues(name).Inc()

		if err := handler.HandleObject(obj); err != nil {
			logrus.Warningf("Sink %s HandleEvent error: %v", name, err)
			descsFailuresTotal.WithLabelValues(name).Inc()
		}
	}
}
