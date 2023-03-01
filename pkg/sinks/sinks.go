// Package sinks ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package sinks

import (
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/newrelic/nri-kube-events/pkg/events"
)

// SinkConfig defines the name and config map of an `events.Sink`
type SinkConfig struct {
	Name   string
	Config map[string]string
}

// MustGetString returns the string variable by the given name.
// If it's not present, an error will given and the application will stop.
func (s SinkConfig) MustGetString(name string) string {
	val, ok := s.Config[name]
	if !ok {
		logrus.Fatalf("Required string variable %s not set for %s Sink", name, s.Name)
	}
	return val
}

// GetDurationOr returns the duration variable by the given name.
// It will return the fallback in case the duration is not found.
// Invalid durations in configuration are not accepted.
func (s SinkConfig) GetDurationOr(name string, fallback time.Duration) time.Duration {
	val, ok := s.Config[name]
	if !ok {
		return fallback
	}

	dur, err := time.ParseDuration(val)
	if err != nil {
		logrus.Fatalf("Duration config field '%s' has invalid value of '%s' for %s Sink: %v", name, val, s.Name, err)
	}

	return dur
}

type sinkFactory func(config SinkConfig, integrationVersion string) (events.Sink, error)

// registeredSinkFactories holds all the registered sinks by this package
var registeredSinkFactories = map[string]sinkFactory{}

func registerSink(name string, factory sinkFactory) {
	if _, ok := registeredSinkFactories[name]; ok {
		logrus.Fatal("registered a double sink factory")
	}

	registeredSinkFactories[name] = factory
}

// CreateSinks takes a slice of SinkConfigs and attempts
// to initialize the sinks.
func CreateSinks(configs []SinkConfig, integrationVersion string) (map[string]events.Sink, error) {

	sinks := make(map[string]events.Sink)

	for _, sinkConf := range configs {

		var ok bool
		var factory sinkFactory

		if factory, ok = registeredSinkFactories[sinkConf.Name]; !ok {
			return sinks, fmt.Errorf("sink not found: %s", sinkConf.Name)
		}

		sink, err := factory(sinkConf, integrationVersion)
		if err != nil {
			return sinks, errors.Wrapf(err, "could not initialize sink %s", sinkConf.Name)
		}

		logrus.Infof("Created sink: %s", sinkConf.Name)

		sinks[sinkConf.Name] = sink
	}

	return sinks, nil
}
