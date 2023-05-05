// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

var testConf = `
workQueueLength: 1337
sinks:
- name: stdout
  config:
    verbose: true
- name: newRelicInfra
  config:
    agentEndpoint: "http://infra-agent.default:8001/v1/data"
    clusterName: "minikube"
`

func TestConfigParse(t *testing.T) {
	workQueueLength := 1337

	tests := []struct {
		serialized string
		parsed     config
	}{
		{
			serialized: testConf,
			parsed: config{
				WorkQueueLength: &workQueueLength,
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
			},
		},
		{
			serialized: "",
			parsed: config{
				WorkQueueLength: nil,
				Sinks:           []sinks.SinkConfig(nil),
			},
		},
	}

	for _, test := range tests {
		conf := strings.NewReader(test.serialized)
		got, err := loadConfig(conf)
		assert.NoError(t, err)
		assert.Equal(t, test.parsed, got)
	}
}
