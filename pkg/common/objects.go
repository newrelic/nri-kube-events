// Package common ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package common

import v1 "k8s.io/api/core/v1"

// KubeEvent represents a Kubernetes event. It specifies if this is the first
// time the event is seen or if it's an update to a previous event.
type KubeEvent struct {
	Verb     string    `json:"verb"`
	Event    *v1.Event `json:"event"`
	OldEvent *v1.Event `json:"old_event,omitempty"`
}
