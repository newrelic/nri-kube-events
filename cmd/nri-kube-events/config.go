// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/newrelic/nri-kube-events/pkg/sinks"
)

type config struct {
	WorkQueueLength *int `yaml:"workQueueLength"`
	Sinks           []sinks.SinkConfig
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
