// Package router ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package router

import "errors"

var ErrInvalidWorkQueueLength = errors.New("new workQueueLength value. Value should be greater than 0")

type Config struct {
	// workQueueLength defines the workQueue's channel backlog.
	// It's needed to handle surges of new objects.
	workQueueLength int
}

// ConfigOption set attributes of the `router.Config`.
type ConfigOption func(*Config) error

func NewConfig(opts ...ConfigOption) (*Config, error) {
	c := &Config{
		workQueueLength: 1024,
	}
	for _, opt := range opts {
		err := opt(c)
		if err != nil {
			return c, err
		}
	}
	return c, nil
}

// WithWorkQueueLength sets the workQueueLength.
// Handle nil values here to make the configuration code more clean.
func WithWorkQueueLength(length *int) ConfigOption {
	return func(rc *Config) error {
		if length == nil {
			return nil
		}

		if *length <= 0 {
			return ErrInvalidWorkQueueLength
		}

		rc.workQueueLength = *length
		return nil
	}
}

func (rc *Config) WorkQueueLength() int {
	return rc.workQueueLength
}
