// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestObjectStateDefaults(t *testing.T) {
	cfg, err := loadConfig(strings.NewReader("objectState:\n  enabled: true\n"))
	require.NoError(t, err)
	assert.True(t, cfg.ObjectState.Enabled)
	assert.Equal(t, 24*time.Hour, cfg.ObjectState.resendAfter())
	assert.Equal(t, 3200.0, cfg.ObjectState.maxRecordsPerSecond())
}

func TestObjectStateParse(t *testing.T) {
	cfg, err := loadConfig(strings.NewReader(`
objectState:
  enabled: true
  resendAfter: "0"
  maxRecordsPerSecond: 250
  clusterName: my-cluster
  namespace: my-namespace
  agentEndpoint: http://localhost:8001/v1/data
  nrStaging: true
  fedramp: true
  proxy: http://proxy.example:3128
`))
	require.NoError(t, err)
	oc := cfg.ObjectState
	assert.True(t, oc.Enabled)
	assert.Equal(t, time.Duration(0), oc.resendAfter())
	assert.Equal(t, 250.0, oc.maxRecordsPerSecond())
	assert.Equal(t, "my-cluster", oc.ClusterName)
	assert.Equal(t, "my-namespace", oc.Namespace)
	assert.Equal(t, "http://localhost:8001/v1/data", oc.AgentEndpoint)
	assert.True(t, oc.NRStaging)
	assert.True(t, oc.Fedramp)
	assert.Equal(t, "http://proxy.example:3128", oc.Proxy)
}

func TestObjectStateResendAfterDuration(t *testing.T) {
	cfg, err := loadConfig(strings.NewReader("objectState:\n  resendAfter: 6h\n"))
	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, cfg.ObjectState.resendAfter())
}

func TestObjectStateAbsentIsDisabled(t *testing.T) {
	cfg, err := loadConfig(strings.NewReader(testConf))
	require.NoError(t, err)
	assert.False(t, cfg.ObjectState.Enabled)
	assert.Equal(t, ObjectStateConfig{}, cfg.ObjectState)
}

func TestObjectStateValidate(t *testing.T) {
	valid := func() ObjectStateConfig {
		return ObjectStateConfig{Enabled: true, ClusterName: "c", AgentEndpoint: "http://localhost:8001/v1/data", Namespace: "ns"}
	}
	require.NoError(t, valid().validate())
	zero := time.Duration(0)
	oc := valid()
	oc.ResendAfter = &zero
	require.NoError(t, oc.validate(), "0 turns the resend off")

	negative := -time.Hour
	for name, mutate := range map[string]func(*ObjectStateConfig){
		"clusterName":         func(c *ObjectStateConfig) { c.ClusterName = "" },
		"agentEndpoint":       func(c *ObjectStateConfig) { c.AgentEndpoint = "" },
		"namespace":           func(c *ObjectStateConfig) { c.Namespace = "" },
		"resendAfter":         func(c *ObjectStateConfig) { c.ResendAfter = &negative },
		"maxRecordsPerSecond": func(c *ObjectStateConfig) { c.MaxRecordsPerSecond = -1 },
	} {
		oc := valid()
		mutate(&oc)
		err := oc.validate()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), name)
	}
}
