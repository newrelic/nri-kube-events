// Package sinks ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package sinks

import (
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/newrelic/nri-kube-events/pkg/common"
)

func init() {
	register("stdout", createStdoutSink)
}

func createStdoutSink(_ SinkConfig, _ string) (Sink, error) {
	return &stdoutSink{}, nil
}

type stdoutSink struct{}

func (stdoutSink) HandleEvent(event common.KubeEvent) error {
	b, err := json.Marshal(event)

	if err != nil {
		return fmt.Errorf("stdoutSink: could not marshal event: %w", err)
	}

	logrus.Info(string(b))
	return nil
}

func (stdoutSink) HandleObject(object common.KubeObject) error {
	b, err := json.Marshal(object)

	if err != nil {
		return fmt.Errorf("stdoutSink: could not marshal object: %w", err)
	}

	logrus.Info(string(b))
	return nil
}
