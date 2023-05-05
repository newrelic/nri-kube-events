// Package sinks ...
// Copyright 2019 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package sinks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdkArgs "github.com/newrelic/infra-integrations-sdk/args"
	sdkEvent "github.com/newrelic/infra-integrations-sdk/data/event"
	sdkIntegration "github.com/newrelic/infra-integrations-sdk/integration"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sethgrid/pester"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"

	"github.com/newrelic/nri-kube-events/pkg/common"
)

func init() {
	register("newRelicInfra", createNewRelicInfraSink)
}

const (
	newRelicNamespace       = "k8s"
	newRelicCategory        = "kubernetes"
	newRelicSDKName         = "kube_events"
	defaultAgentHTTPTimeout = time.Second * 10
)

func createNewRelicInfraSink(config SinkConfig, integrationVersion string) (Sink, error) {
	clusterName := config.MustGetString("clusterName")
	agentEndpoint := config.MustGetString("agentEndpoint")
	agentHTTPTimeout := config.GetDurationOr("agentHTTPTimeout", defaultAgentHTTPTimeout)

	args := struct {
		sdkArgs.DefaultArgumentList
		ClusterName string `help:"Identifier of your cluster. You could use it later to filter data in your New Relic account"`
	}{
		ClusterName: clusterName,
	}

	i, err := sdkIntegration.New(newRelicSDKName, integrationVersion, sdkIntegration.Args(&args))
	if err != nil {
		return nil, fmt.Errorf("error while initializing New Relic SDK integration: %w", err)
	}

	logrus.Debugf("NewRelic sink configuration: agentTimeout=%s, clusterName=%s, agentEndpoint=%s",
		agentHTTPTimeout,
		clusterName,
		agentEndpoint,
	)

	p := pester.New()
	p.Backoff = pester.ExponentialBackoff
	p.LogHook = func(e pester.ErrEntry) {
		logrus.Debugf("Pester HTTP error: %#v", e)
	}
	// 32 is semi-randomly chosen. It should be high enough not to block events coming from the k8s API,
	// but not too high, because the number is directly related to the amount of goroutines that are running.
	p.Concurrency = 32
	p.MaxRetries = 3

	return &newRelicInfraSink{
		pesterClient:   p,
		clusterName:    clusterName,
		sdkIntegration: i,
		agentEndpoint:  agentEndpoint,
		metrics:        createNewRelicInfraSinkMetrics(),
	}, nil
}

func createNewRelicInfraSinkMetrics() newRelicInfraSinkMetrics {
	return newRelicInfraSinkMetrics{
		httpTotalFailures: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "nr",
			Subsystem: "kube_events",
			Name:      "infra_sink_http_failures_total",
			Help:      "Total amount of http failures connecting to the Agent",
		}),
		httpResponses: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nr",
			Subsystem: "kube_events",
			Name:      "infra_sink_http_responses_total",
			Help:      "Total amount of http responses, per code, from the New Relic Infra Agent",
		}, []string{"code"}),
	}
}

type newRelicInfraSinkMetrics struct {
	httpTotalFailures prometheus.Counter
	httpResponses     *prometheus.CounterVec
}

// The newRelicInfraSink implements the Sink interface.
// It will forward all events to the locally running Relic Infrastructure Agent
type newRelicInfraSink struct {
	pesterClient   *pester.Client
	sdkIntegration *sdkIntegration.Integration
	clusterName    string
	agentEndpoint  string
	metrics        newRelicInfraSinkMetrics
}

// HandleEvent sends the event to the New Relic Agent
func (ns *newRelicInfraSink) HandleEvent(kubeEvent common.KubeEvent) error {
	defer ns.sdkIntegration.Clear()

	e, err := ns.createEntity(kubeEvent)

	if err != nil {
		return fmt.Errorf("unable to create entity: %w", err)
	}

	flattenedEvent, err := common.FlattenStruct(kubeEvent)

	if err != nil {
		return fmt.Errorf("could not flatten EventData struct: %w", err)
	}

	ns.decorateEvent(flattenedEvent)

	event := sdkEvent.NewWithAttributes(
		kubeEvent.Event.Message,
		newRelicCategory,
		flattenedEvent,
	)
	err = e.AddEvent(event)
	if err != nil {
		return fmt.Errorf("couldn't add event: %w", err)
	}

	err = ns.sendIntegrationPayloadToAgent()
	if err != nil {
		return fmt.Errorf("error sending data to agent: %w", err)
	}

	return nil
}

// createEntity creates the entity related to the event.
func (ns *newRelicInfraSink) createEntity(kubeEvent common.KubeEvent) (*sdkIntegration.Entity, error) {
	entityType, entityName := formatEntityID(ns.clusterName, kubeEvent)

	e, err := ns.sdkIntegration.Entity(entityName, entityType)
	if err != nil {
		return nil, fmt.Errorf("could not initialize new SDK Entity: %w", err)
	}

	return e, nil
}

// formatEntity returns an entity id information as tuple of (entityType, entityName).
//
// Returned values should be structured as follows:
// (k8s:<cluster_name>:<namespace(optional)>:<object_type>, <object_name>)
//
// Example pod:
// ("k8s:fsi-cluster-explorer:default:pod", "newrelic-infra-s2wh9")
//
// Example node entityName:
// ("k8s:fsi-cluster-explorer:node", "worker-node-1")
func formatEntityID(clusterName string, kubeEvent common.KubeEvent) (string, string) {
	parts := []string{newRelicNamespace}

	parts = append(parts, clusterName)

	if kubeEvent.Event.InvolvedObject.Namespace != "" {
		parts = append(parts, kubeEvent.Event.InvolvedObject.Namespace)
	}

	parts = append(parts, strings.ToLower(kubeEvent.Event.InvolvedObject.Kind))

	return strings.Join(parts, ":"), kubeEvent.Event.InvolvedObject.Name
}

func (ns *newRelicInfraSink) sendIntegrationPayloadToAgent() error {
	jsonBytes, err := json.Marshal(ns.sdkIntegration)
	if err != nil {
		return fmt.Errorf("unable to marshal data: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", ns.agentEndpoint, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return fmt.Errorf("unable to prepare request: %w", err)
	}

	resp, err := ns.pesterClient.Do(request)

	if err != nil {
		ns.metrics.httpTotalFailures.Inc()
		return fmt.Errorf("HTTP transport error: %w", err)
	}

	disposeBody(resp)

	ns.metrics.httpResponses.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected statuscode:%s, expected: 204 No Content", resp.Status)
	}

	return nil
}

// disposeBody reads the entire http response body and closes it after.
// This is a performance optimisation. According to the docs:
//
// https://golang.org/pkg/net/http/#Client.Do
// If the returned error is nil, the Response will contain a non-nil Body which the user is expected to close.
// If the Body is not both read to EOF and closed, the Client's underlying RoundTripper (typically Transport)
// may not be able to re-use a persistent TCP connection to the server for a subsequent "keep-alive" request.
func disposeBody(response *http.Response) {
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		logrus.Debugf("warning: could not discard response body: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		logrus.Debugf("warning: could not close response body: %v", err)
	}
}

func (ns *newRelicInfraSink) decorateEvent(flattenedEvent map[string]interface{}) {
	flattenedEvent["eventRouterVersion"] = ns.sdkIntegration.IntegrationVersion
	flattenedEvent["integrationVersion"] = ns.sdkIntegration.IntegrationVersion
	flattenedEvent["integrationName"] = ns.sdkIntegration.Name
	flattenedEvent["clusterName"] = ns.clusterName
}
