// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package sinks

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/newrelic/nri-kube-events/pkg/events"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFormatEntityID(t *testing.T) {
	podObject := v1.ObjectReference{
		Kind:      "Pod",
		Namespace: "test_namespace",
		Name:      "TestPod",
	}

	nodeObject := v1.ObjectReference{
		Kind: "Node",
		Name: "Worker1c",
	}

	tt := []struct {
		involvedObject                                      v1.ObjectReference
		expectedEntityType, expectedEntityName, clusterName string
	}{
		{
			involvedObject:     podObject,
			expectedEntityType: "k8s:test_cluster:test_namespace:pod",
			expectedEntityName: "TestPod",
			clusterName:        "test_cluster",
		},
		{
			involvedObject:     podObject,
			expectedEntityType: "k8s:different_cluster_name:test_namespace:pod",
			expectedEntityName: "TestPod",
			clusterName:        "different_cluster_name",
		},
		{
			involvedObject:     nodeObject,
			expectedEntityType: "k8s:my_cluster:node",
			expectedEntityName: "Worker1c",
			clusterName:        "my_cluster",
		},
	}

	for i, testCase := range tt {

		entityType, entityName := formatEntityID(
			testCase.clusterName,
			events.KubeEvent{
				Event: &v1.Event{
					InvolvedObject: testCase.involvedObject,
				},
			},
		)

		if diff := cmp.Diff(entityName, testCase.expectedEntityName); diff != "" {
			t.Errorf("[%d] formatEntityID() name mismatch (-want +got):\n%s", i, diff)
		}
		if diff := cmp.Diff(entityType, testCase.expectedEntityType); diff != "" {
			t.Errorf("[%d] formatEntityID() type mismatch (-want +got):\n%s", i, diff)
		}
	}
}

func TestNewRelicSinkIntegration(t *testing.T) {
	_ = os.Setenv("METADATA", "true")
	_ = os.Setenv("NRI_KUBE_EVENTS_myCustomAttribute", "attrValue")
	defer os.Clearenv()
	expectedPostJSON, err := ioutil.ReadFile("./testdata/new_relic_infra_test_data.json")
	if err != nil {
		t.Fatalf("could not read test_post_data.json: %v", err)
	}
	var expectedData interface{}
	if err = json.Unmarshal(expectedPostJSON, &expectedData); err != nil {
		t.Fatalf("error unmarshalling test_post_data.json: %v", err)
	}

	responseHandler := func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)

		defer func() {
			_ = r.Body.Close()
		}()

		if err != nil {
			t.Fatalf("error reading request body: %v", err)
		}

		var postData interface{}
		if err = json.Unmarshal(body, &postData); err != nil {
			t.Fatalf("error unmarshalling request body: %v", err)
		}

		if diff := cmp.Diff(expectedData, postData); diff != "" {
			t.Errorf("request mismatch (-want +got):\n%s", diff)
		}

		w.WriteHeader(http.StatusNoContent)
	}
	var testServer = httptest.NewServer(http.HandlerFunc(responseHandler))

	config := SinkConfig{
		Config: map[string]string{
			"clusterName":   "test-cluster",
			"agentEndpoint": testServer.URL,
		},
	}
	// This time is fixed since it is the one added in expected data
	now, _ := time.Parse(time.RFC3339, "2021-03-12T10:55:43Z")

	sink, _ := createNewRelicInfraSink(config)

	err = sink.HandleEvent(events.KubeEvent{
		Verb: "ADDED",
		Event: &v1.Event{
			Message: "The event message",
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
				Name:      "TestPod",
			},
			LastTimestamp: metav1.NewTime(now),
		}})
	if err != nil {
		t.Errorf("unexpected error handling event: %v", err)
	}
}

func TestNewRelicInfraSink_HandleEvent_AddEventError(t *testing.T) {
	t.Skip("Speak to OHAI about global flags automatically registered when we call integration.New")
	config := SinkConfig{
		Config: map[string]string{
			"clusterName":   "test-cluster",
			"agentEndpoint": "",
		},
	}
	sink, _ := createNewRelicInfraSink(config)
	err := sink.HandleEvent(events.KubeEvent{
		Verb: "ADDED",
		Event: &v1.Event{
			Message: "",
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
				Name:      "TestPod",
			},
		}})
	if err == nil {
		t.Fatal("expected error, got nothing")
	}

	wantedError := "couldn't add event"
	if !strings.Contains(err.Error(), wantedError) {
		t.Errorf("wanted error with message '%s' got: '%v'", wantedError, err)
	}
}

func TestFlattenStruct(t *testing.T) {
	// This time is fixed since it is the one added in expected data
	now, _ := time.Parse(time.RFC3339, "2021-03-12T10:55:43Z")

	got, _ := flattenStruct(events.KubeEvent{Verb: "UPDATE", Event: &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			Labels: map[string]string{
				"test_label1": "test_value1",
				"test_label2": "test_value2",
			},
			Finalizers:        []string{"1", "2"},
			CreationTimestamp: metav1.NewTime(now),
		},
		Count: 10,
		InvolvedObject: v1.ObjectReference{
			Kind:      "Pod",
			Namespace: "test_namespace",
		},
		LastTimestamp: metav1.NewTime(now),
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
		"event.lastTimestamp":               "2021-03-12T10:55:43Z",
		"verb":                              "UPDATE",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("flattenStruct() mismatch (-want +got):\n%s", diff)
	}
}
