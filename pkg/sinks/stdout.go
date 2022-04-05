// Package sinks ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package sinks

import (
	"encoding/json"

	"github.com/pkg/errors"

	"github.com/newrelic/nri-kube-events/pkg/events"
	"github.com/sirupsen/logrus"
)

func init() {
	registerSink("stdout", createStdoutSink)
}

func createStdoutSink(_ SinkConfig, _ string) (events.Sink, error) {
	return &stdoutSink{}, nil
}

type stdoutSink struct{}

func (stdoutSink) HandleEvent(event events.KubeEvent) error {
	b, err := json.Marshal(event)

	if err != nil {
		return errors.Wrap(err, "stdoutSink: could not marshal event")
	}

	logrus.Infof(string(b))
	return nil
}
