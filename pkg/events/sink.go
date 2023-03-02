// Package events ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package events

import (
	"time"

	"github.com/newrelic/nri-kube-events/pkg/common"
	"github.com/prometheus/client_golang/prometheus"
)

// Sink receives events from the router, process and publish them to a certain
// destination (stdout, NewRelic platform, etc.).
type Sink interface {
	HandleEvent(kubeEvent common.KubeEvent) error
}

type observedSink struct {
	sink     Sink
	observer prometheus.Observer
}

func (o *observedSink) HandleEvent(kubeEvent common.KubeEvent) error {
	t := time.Now()
	defer func() { o.observer.Observe(time.Since(t).Seconds()) }()

	return o.sink.HandleEvent(kubeEvent)
}
