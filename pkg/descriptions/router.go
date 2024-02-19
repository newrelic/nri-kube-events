// Package descriptions ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package descriptions

import (
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/newrelic/nri-kube-events/pkg/common"
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
	HandleObject(kubeEvent common.KubeObject) error
}

// Router listens for events coming from a SharedIndexInformer,
// and forwards them to the registered sinks
type Router struct {
	// list of handlers to send events to
	handlers map[string]ObjectHandler

	// all updates & adds will be appended to this queue
	workQueue chan common.KubeObject

	// all events will be filtered by this list of filters
	excludeFilterProgramms []*vm.Program
}

type observedObjectHandler struct {
	ObjectHandler
	prometheus.Observer
}

func (o *observedObjectHandler) HandleObject(kubeObject common.KubeObject) error {
	t := time.Now()
	defer func() { o.Observer.Observe(time.Since(t).Seconds()) }()

	return o.ObjectHandler.HandleObject(kubeObject)
}

// NewRouter returns a new Router which listens to the given SharedIndexInformer,
// and forwards all incoming events to the given sinks
func NewRouter(informers []cache.SharedIndexInformer, handlers map[string]ObjectHandler, filters []string, opts ...router.ConfigOption) *Router {
	config, err := router.NewConfig(opts...)
	if err != nil {
		logrus.Fatalf("Error with Router configuration: %v", err)
	}

	workQueue := make(chan common.KubeObject, config.WorkQueueLength())

	for _, informer := range informers {
		_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				workQueue <- common.KubeObject{
					Obj:  obj.(runtime.Object),
					Verb: "ADDED",
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				workQueue <- common.KubeObject{
					Obj:    newObj.(runtime.Object),
					OldObj: oldObj.(runtime.Object),
					Verb:   "UPDATE",
				}
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

	env := map[string]interface{}{
		"e": common.KubeObject{},
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
		case event := <-r.workQueue:
			r.publishObjectDescription(event)
		}
	}
}

func (r *Router) publishObjectDescription(kubeObject common.KubeObject) {
	if r.FilterObject(kubeObject) {
		logrus.Debugf("Filtered object: %v", kubeObject)
		return
	}

	for name, handler := range r.handlers {
		descsReceivedTotal.WithLabelValues(name).Inc()

		if err := handler.HandleObject(kubeObject); err != nil {
			logrus.Warningf("Sink %s HandleEvent error: %v", name, err)
			descsFailuresTotal.WithLabelValues(name).Inc()
		}
	}
}

func (r *Router) FilterObject(event common.KubeObject) bool {
	env := map[string]interface{}{
		"o": event,
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
