// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

const (
	DefaultDescribeRefresh = 24 * time.Hour

	defaultObjectStateResendAfter         = 24 * time.Hour
	defaultObjectStateMaxRecordsPerSecond = 100
)

type config struct {
	WorkQueueLength *int `yaml:"workQueueLength"`
	Sinks           []sinks.SinkConfig

	CaptureEvents   *bool          `yaml:"captureEvents"`
	CaptureDescribe *bool          `yaml:"captureDescribe"`
	DescribeRefresh *time.Duration `yaml:"describeRefresh"`

	// ObjectState configures the object-state carrier. It is off unless enabled is true.
	ObjectState ObjectStateConfig `yaml:"objectState"`
}

// ObjectStateConfig configures the object-state carrier, which sends the state of every
// object in the cluster through the infrastructure agent forwarder. The license key comes
// from the NEW_RELIC_LICENSE_KEY env var, never from this file.
type ObjectStateConfig struct {
	// ResendAfter nil means 24h; 0 turns the resend of unchanged objects off.
	ResendAfter *time.Duration `yaml:"resendAfter"`
	ClusterName string         `yaml:"clusterName"`
	// Namespace holds the hash store ConfigMaps and the HMAC key Secret.
	Namespace string `yaml:"namespace"`
	// AgentEndpoint is the forwarder's /v1/data URL.
	AgentEndpoint string `yaml:"agentEndpoint"`
	// Proxy is the proxy URL for the command API poll. Empty means no proxy.
	Proxy string `yaml:"proxy"`
	// MaxRecordsPerSecond 0 means 100.
	MaxRecordsPerSecond float64 `yaml:"maxRecordsPerSecond"`
	Enabled             bool    `yaml:"enabled"`
	NRStaging           bool    `yaml:"nrStaging"`
	Fedramp             bool    `yaml:"fedramp"`
}

// validate reports the first setting that keeps the carrier from starting. A negative
// resendAfter or maxRecordsPerSecond is rejected, where the defaults would otherwise turn
// the resend off or replace the rate without a word.
func (c ObjectStateConfig) validate() error {
	switch {
	case c.ClusterName == "":
		return errors.New("objectState.enabled is true but objectState.clusterName is empty")
	case c.AgentEndpoint == "":
		return errors.New("objectState.enabled is true but objectState.agentEndpoint is empty")
	case c.Namespace == "":
		return errors.New("objectState.enabled is true but objectState.namespace is empty")
	case c.ResendAfter != nil && *c.ResendAfter < 0:
		return fmt.Errorf("objectState.resendAfter must not be negative, got %s", *c.ResendAfter)
	case c.MaxRecordsPerSecond < 0:
		return fmt.Errorf("objectState.maxRecordsPerSecond must not be negative, got %v", c.MaxRecordsPerSecond)
	}
	return nil
}

func (c ObjectStateConfig) resendAfter() time.Duration {
	if c.ResendAfter == nil {
		return defaultObjectStateResendAfter
	}
	return *c.ResendAfter
}

func (c ObjectStateConfig) maxRecordsPerSecond() float64 {
	if c.MaxRecordsPerSecond <= 0 {
		return defaultObjectStateMaxRecordsPerSecond
	}
	return c.MaxRecordsPerSecond
}

func loadConfig(file io.Reader) (config, error) {
	var cfg config

	contents, err := io.ReadAll(file)
	if err != nil {
		return cfg, fmt.Errorf("could not read configuration file: %w", err)
	}

	err = yaml.Unmarshal(contents, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("could not parse configuration file: %w", err)
	}

	return cfg, nil
}

func mustLoadConfigFile(configFile string) config {
	f, err := os.Open(configFile)
	if err != nil {
		logrus.Fatalf("could not open configuration file: %v", err)
	}

	cfg, err := loadConfig(f)

	if errClose := f.Close(); errClose != nil {
		logrus.Warningf("error closing config file: %v", errClose)
	}

	if err != nil {
		logrus.Fatalf("could not parse configuration file: %v", err)
	}

	return cfg
}
