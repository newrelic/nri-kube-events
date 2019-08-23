// Package events ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package events

import "fmt"

type routerConfig struct {
	// workQueueLength defines the workQueue's channel backlog. It's needed to handle surges of new events.
	workQueueLength int
}

// RouterConfigOption set attributes of the `routerConfig`.
type RouterConfigOption func(rc *routerConfig) error

// WithWorkQueueLength sets the workQueueLength
// Handle nil values here to make the configuration code more clean
func WithWorkQueueLength(length *int) RouterConfigOption {
	return func(rc *routerConfig) error {
		if length == nil {
			return nil
		}

		if *length <= 0 {
			return fmt.Errorf("invalid workQueueLength value of %d. Value should be greater than 0", *length)
		}

		rc.workQueueLength = *length
		return nil
	}
}
