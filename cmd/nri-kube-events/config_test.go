// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

var testConf = `
captureEvents: false
captureDescribe: true
describeRefresh: 3h
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
	captureEvents := false
	captureDescribe := true
	describeRefresh := 3 * time.Hour
	workQueueLength := 1337

	tests := []struct {
		serialized string
		parsed     config
	}{
		{
			serialized: testConf,
			parsed: config{
				CaptureEvents:   &captureEvents,
				CaptureDescribe: &captureDescribe,
				DescribeRefresh: &describeRefresh,
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
