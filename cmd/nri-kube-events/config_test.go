// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

func TestConfigParse(t *testing.T) {
	conf := strings.NewReader(`
workQueueLength: 1337
sinks:
- name: stdout
  config:
    verbose: true
- name: newRelicInfra
  config:
    agentEndpoint: "http://infra-agent.default:8001/v1/data"
    clusterName: "minikube"
`)

	got, err := loadConfig(conf)

	if err != nil {
		t.Fatalf("unexpected error while parsing config: %v", err)
	}

	want := config{
		WorkQueueLength: intPtr(1337),
		Sinks: []sinks.SinkConfig{
			{
				Name: "stdout",
				Config: map[string]string{
					"verbose": "true",
				},
			},
			{
				Name: "newRelicInfra",
				Config: map[string]string{
					"clusterName":   "minikube",
					"agentEndpoint": "http://infra-agent.default:8001/v1/data",
				},
			},
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("loadConfig() mismatch (-want +got):\n%s", diff)
	}
}

func TestNilValues(t *testing.T) {
	var b bytes.Reader
	got, err := loadConfig(&b)

	if err != nil {
		t.Fatalf("unexpected error while parsing config: %v", err)
	}

	want := config{
		WorkQueueLength: nil,
		Sinks:           []sinks.SinkConfig(nil),
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("loadConfig() mismatch (-want +got):\n%s", diff)
	}
}

func intPtr(val int) *int { return &val }
