// Package sinks ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package sinks

import (
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/newrelic/nri-kube-events/pkg/events"
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
		return fmt.Errorf("stdoutSink: could not marshal event: %w", err)
	}

	logrus.Infof(string(b))
	return nil
}
