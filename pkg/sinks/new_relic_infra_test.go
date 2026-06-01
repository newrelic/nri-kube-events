// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package sinks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/newrelic/nri-kube-events/pkg/common"
)

func TestFormatEntityID(t *testing.T) {
	podObject := corev1.ObjectReference{
		Kind:      "Pod",
		Namespace: "test_namespace",
		Name:      "TestPod",
	}

	nodeObject := corev1.ObjectReference{
		Kind: "Node",
		Name: "Worker1c",
	}

	tt := []struct {
		involvedObject                                      corev1.ObjectReference
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

	for _, testCase := range tt {
		entityType, entityName := formatEntityID(
			testCase.clusterName,
			common.KubeEvent{
				Event: &corev1.Event{
					InvolvedObject: testCase.involvedObject,
				},
			},
		)

		assert.Equal(t, testCase.expectedEntityName, entityName)
		assert.Equal(t, testCase.expectedEntityType, entityType)
	}
}

func TestNewRelicSinkIntegration_HandleEvent_Success(t *testing.T) {
	_ = os.Setenv("METADATA", "true")
	_ = os.Setenv("NRI_KUBE_EVENTS_myCustomAttribute", "attrValue")
	defer os.Clearenv()
	expectedPostJSON, err := os.ReadFile("./testdata/event_data.json")
	if err != nil {
		t.Fatalf("could not read test_post_data.json: %v", err)
	}
	var expectedData interface{}
	if err = json.Unmarshal(expectedPostJSON, &expectedData); err != nil {
		t.Fatalf("error unmarshalling test_post_data.json: %v", err)
	}

	responseHandler := func(w http.ResponseWriter, r *http.Request) {
		body, err2 := io.ReadAll(r.Body)

		defer func() {
			_ = r.Body.Close()
		}()

		if err2 != nil {
			t.Fatalf("error reading request body: %v", err2)
		}

		var postData interface{}
		if err2 = json.Unmarshal(body, &postData); err2 != nil {
			t.Fatalf("error unmarshalling request body: %v", err2)
		}

		assert.Equal(t, expectedData, postData)
		w.WriteHeader(http.StatusNoContent)
	}
	var testServer = httptest.NewServer(http.HandlerFunc(responseHandler))

	config := SinkConfig{
		Config: map[string]string{
			"clusterName":   "test-cluster",
			"agentEndpoint": testServer.URL,
		},
	}
	sink, _ := createNewRelicInfraSink(config, "0.0.0")
	err = sink.HandleEvent(common.KubeEvent{
		Verb: "ADDED",
		Event: &corev1.Event{
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
			InvolvedObject: corev1.ObjectReference{
				Kind:      "Pod",
				Namespace: "test_namespace",
				Name:      "TestPod",
			},
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
	sink, _ := createNewRelicInfraSink(config, "0.0.0")
	err := sink.HandleEvent(common.KubeEvent{
		Verb: "ADDED",
		Event: &corev1.Event{
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
			InvolvedObject: corev1.ObjectReference{
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

func TestSerialize(t *testing.T) {
	tests := []struct {
		inputObj       runtime.Object
		validateResult func(t *testing.T, output string)
		name           string
		wantErr        bool
	}{
		{
			name: "Standard pod",
			inputObj: &corev1.Pod{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Pod",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-pod",
				},
			},
			validateResult: func(t *testing.T, actual string) {
				expected := `{
    "kind": "Pod",
    "apiVersion": "v1",
    "metadata": {
        "name": "my-pod"
    },
    "spec": {
        "containers": null
    },
    "status": {}
}
`
				assert.JSONEq(t, expected, actual)
			},
			wantErr: false,
		},
		{
			name: "Secret",
			inputObj: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-secret",
				},
				Data: map[string][]byte{
					"password": []byte("super-secret-base64"),
				},
				StringData: map[string]string{
					"api-key": "raw-api-token",
				},
			},
			validateResult: func(t *testing.T, actual string) {
				expected := `{
    "kind": "Secret",
    "apiVersion": "v1",
    "metadata": {
        "name": "my-secret"
    },
    "data": {
        "password": "UkVEQUNURUQ="
    },
    "stringData": {
        "api-key": "REDACTED"
    }
}
`
				assert.JSONEq(t, expected, actual)
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := safelySerializeK8sObjectToJSON(tt.inputObj)
			if (err != nil) != tt.wantErr {
				t.Errorf("safelySerializeK8sObjectToJSON() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && tt.validateResult != nil {
				tt.validateResult(t, got)
			}
		})
	}
}
