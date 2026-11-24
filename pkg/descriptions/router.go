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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	descsSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nr",
		Subsystem: "k8s_descriptions",
		Name:      "skipped",
		Help:      "Total amount of objects not described because object state collects their resource",
	}, []string{"resource"})
	descsReplayedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nr",
		Subsystem: "k8s_descriptions",
		Name:      "replayed",
		Help:      "Total amount of cached objects described again when object state stopped collecting their resource",
	}, []string{"resource"})
)

type ObjectHandler interface {
	HandleObject(kubeEvent common.KubeObject) error
}

// Informer is a describe informer and the resource it watches.
type Informer struct {
	cache.SharedIndexInformer
	Resource schema.GroupResource
}

// Coverage reports the resources that another collector already sends. *objects.Coverage
// satisfies it.
type Coverage interface {
	// Covers reports whether the other collector sends gr.
	Covers(gr schema.GroupResource) bool
	// Changed receives after Covers changed. Changes may collapse into one receive.
	Changed() <-chan struct{}
}

// Router listens for events coming from a SharedIndexInformer,
// and forwards them to the registered sinks
type Router struct {
	// list of handlers to send events to
	handlers map[string]ObjectHandler

	// all updates & adds will be appended to this queue
	workQueue chan queuedObject

	// coverage is nil when every object is described.
	coverage Coverage
	// covered is the router's view of coverage. Only Run reads and writes it.
	covered   map[schema.GroupResource]bool
	informers []Informer
}

// queuedObject is an object and the resource of the informer that delivered it.
type queuedObject struct {
	obj      common.KubeObject
	resource schema.GroupResource
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

// NewRouter returns a new Router which listens to the given informers, and forwards all
// incoming events to the given sinks. With a non-nil coverage, the Router skips the objects
// of every resource that coverage covers, and when a resource stops being covered it
// describes every object in that resource's informer cache once. A nil coverage describes
// every object.
func NewRouter(informers []Informer, handlers map[string]ObjectHandler, coverage Coverage, opts ...router.ConfigOption) *Router {
	config, err := router.NewConfig(opts...)
	if err != nil {
		logrus.Fatalf("Error with Router configuration: %v", err)
	}

	workQueue := make(chan queuedObject, config.WorkQueueLength())

	for _, informer := range informers {
		resource := informer.Resource
		_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				workQueue <- queuedObject{resource: resource, obj: common.KubeObject{
					Obj:  obj.(runtime.Object),
					Verb: "ADDED",
				}}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				workQueue <- queuedObject{resource: resource, obj: common.KubeObject{
					Obj:    newObj.(runtime.Object),
					OldObj: oldObj.(runtime.Object),
					Verb:   "UPDATE",
				}}
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
		informers: informers,
		coverage:  coverage,
		covered:   map[schema.GroupResource]bool{},
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

	var changed <-chan struct{}
	if r.coverage != nil {
		changed = r.coverage.Changed()
		r.applyCoverage(stopChan)
	}
	for {
		select {
		case <-stopChan:
			return
		case <-changed:
			r.applyCoverage(stopChan)
		case event := <-r.workQueue:
			// A coverage change signaled before this object is described applies to it.
			select {
			case <-changed:
				r.applyCoverage(stopChan)
			default:
			}
			if r.covered[event.resource] {
				descsSkippedTotal.WithLabelValues(event.resource.String()).Inc()
				continue
			}
			r.publishObjectDescription(event.obj)
		}
	}
}

// applyCoverage brings the router's view of coverage up to date. For every resource that
// was covered and no longer is, it describes every object in the resource's informer cache,
// so the current state of each object is sent even when its last change arrived while it
// was covered. It stops early once stopChan is closed.
func (r *Router) applyCoverage(stopChan <-chan struct{}) {
	for _, inf := range r.informers {
		select {
		case <-stopChan:
			return
		default:
		}
		now := r.coverage.Covers(inf.Resource)
		was := r.covered[inf.Resource]
		r.covered[inf.Resource] = now
		switch {
		case now && !was:
			logrus.Infof("Object state collects %s; skipping its descriptions", inf.Resource)
		case was && !now:
			r.replay(inf, stopChan)
		}
	}
}

// replay describes every object in inf's cache once, as an UPDATE with OldObj equal to Obj,
// which is what an informer resync delivers.
func (r *Router) replay(inf Informer, stopChan <-chan struct{}) {
	cached := inf.GetStore().List()
	logrus.Infof("Object state stopped collecting %s; describing its %d cached objects", inf.Resource, len(cached))
	for _, item := range cached {
		select {
		case <-stopChan:
			return
		default:
		}
		obj, ok := item.(runtime.Object)
		if !ok {
			continue
		}
		descsReplayedTotal.WithLabelValues(inf.Resource.String()).Inc()
		r.publishObjectDescription(common.KubeObject{Obj: obj, OldObj: obj, Verb: "UPDATE"})
	}
}

func (r *Router) publishObjectDescription(kubeObject common.KubeObject) {
	for name, handler := range r.handlers {
		descsReceivedTotal.WithLabelValues(name).Inc()

		if err := handler.HandleObject(kubeObject); err != nil {
			logrus.Warningf("Sink %s HandleEvent error: %v", name, err)
			descsFailuresTotal.WithLabelValues(name).Inc()
		}
	}
}
