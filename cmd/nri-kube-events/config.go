// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"io"
	"io/ioutil"
	"os"

	"github.com/newrelic/nri-kube-events/pkg/sinks"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type config struct {
	WorkQueueLength *int `yaml:"workQueueLength"`
	Sinks           []sinks.SinkConfig
}

func loadConfig(file io.Reader) (config, error) {

	var cfg config

	contents, err := ioutil.ReadAll(file)
	if err != nil {
		return cfg, errors.Wrap(err, "could not read configuration file")
	}

	err = yaml.Unmarshal(contents, &cfg)

	return cfg, errors.Wrap(err, "could not parse configuration file")
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
