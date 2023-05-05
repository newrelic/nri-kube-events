// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package common_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/newrelic/nri-kube-events/pkg/common"
)

func TestLimitSplit(t *testing.T) {
	tests := []struct {
		input  string
		limit  int
		output []string
	}{
		{
			input:  "short string",
			limit:  20,
			output: []string{"short string"},
		},
		{
			input:  "very very very long string",
			limit:  20,
			output: []string{"very very very long ", "string"},
		},
		{
			input:  "",
			limit:  20,
			output: nil,
		},
		{
			input:  "short",
			limit:  0,
			output: []string{"short"},
		},
		{
			input:  "日本語",
			limit:  4,
			output: []string{"日", "本", "語"},
		},
		{
			input:  "bad utf8 \xbd\xb2\x3d\xbc\x20\xe2\x8c\x98",
			limit:  8,
			output: []string{"bad utf8", " \xbd\xb2\x3d\xbc\x20", "\xe2\x8c\x98"},
		},
	}

	for _, test := range tests {
		assert.Equal(t, test.output, common.LimitSplit(test.input, test.limit))
	}
}

func TestFlattenStruct(t *testing.T) {
	got, _ := common.FlattenStruct(common.KubeEvent{Verb: "UPDATE", Event: &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			Labels: map[string]string{
				"test_label1": "test_value1",
				"test_label2": "test_value2",
			},
			Finalizers: []string{"1", "2"},
		},
		Count: 10,
		InvolvedObject: v1.ObjectReference{
			Kind:      "Pod",
			Namespace: "test_namespace",
		},
	}})

	want := map[string]interface{}{
		"event.count":                       float64(10),
		"event.metadata.name":               "test",
		"event.metadata.labels.test_label1": "test_value1",
		"event.metadata.labels.test_label2": "test_value2",
		"event.involvedObject.kind":         "Pod",
		"event.involvedObject.namespace":    "test_namespace",
		"event.metadata.finalizers[0]":      "1",
		"event.metadata.finalizers[1]":      "2",
		"verb":                              "UPDATE",
	}

	assert.Equal(t, want, got)
}
