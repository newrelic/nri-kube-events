package e2e

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"time"

	sdkEvent "github.com/newrelic/infra-integrations-sdk/data/event"
	"github.com/newrelic/nri-kube-events/pkg/events"
	"github.com/newrelic/nri-kube-events/pkg/sinks"
	log "github.com/sirupsen/logrus"
)

// Must be in sync with unexported name in pkg/sinks/new_relic_infra.go:32.
const newRelicInfraSinkID = "newRelicInfra"

// MockedAgentSink is an instrumented infra-agent sink for testing e2e reception and processing.
type MockedAgentSink struct {
	agentSink         events.Sink
	httpServer        *httptest.Server
	eventReceivedChan chan struct{}
	receivedEvents    []sdkEvent.Event
	mtx               *sync.RWMutex
}

// NewMockedAgentSink returns an instrumented infra-agent sink for testing.
func NewMockedAgentSink() *MockedAgentSink {
	mockedAgentSink := &MockedAgentSink{
		mtx:               &sync.RWMutex{},
		eventReceivedChan: make(chan struct{}, 8),
	}
	mockedAgentSink.httpServer = httptest.NewServer(mockedAgentSink)

	agentSinkConfig := sinks.SinkConfig{
		Name: newRelicInfraSinkID,
		Config: map[string]string{
			"clusterName":   "integrationTest",
			"agentEndpoint": "http://" + mockedAgentSink.httpServer.Listener.Addr().String(),
		},
	}

	createdSinks, err := sinks.CreateSinks([]sinks.SinkConfig{agentSinkConfig})
	if err != nil {
		log.Fatalf("error creating infra sink: %v", err)
	}

	agentSink, ok := createdSinks[newRelicInfraSinkID]
	if !ok {
		log.Fatal("could not retrieve agent infra sink from map")
	}

	mockedAgentSink.agentSink = agentSink

	return mockedAgentSink
}

// HandleEvent sends a notification to the event received channel and then forwards it to the underlying sink.
func (mas *MockedAgentSink) HandleEvent(kubeEvent events.KubeEvent) error {
	mas.eventReceivedChan <- struct{}{}
	return mas.agentSink.HandleEvent(kubeEvent)
}

// ServeHTTP handles a request that would be for the infra-agent and stores the unmarshalled event.
func (mas *MockedAgentSink) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	mas.mtx.Lock()
	defer mas.mtx.Unlock()

	var ev struct {
		Data []struct {
			Events []sdkEvent.Event `json:"events"`
		} `json:"data"`
	}

	defer r.Body.Close() // nolint:errcheck
	body, _ := ioutil.ReadAll(r.Body)
	err := json.Unmarshal(body, &ev)
	if err != nil {
		log.Fatalf("error getting request body")
	}

	if len(ev.Data) == 0 {
		log.Warnf("received payload with no data: %s", string(body))
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	mas.receivedEvents = append(mas.receivedEvents, ev.Data[0].Events...)
	rw.WriteHeader(http.StatusNoContent) // The the agent does
}

// Has relaxedly checks whether the mocked agent has received an event.
func (mas *MockedAgentSink) Has(testEvent *sdkEvent.Event) bool {
	mas.mtx.RLock()
	defer mas.mtx.RUnlock()

	for i := range mas.receivedEvents {
		receivedEvent := &mas.receivedEvents[i]

		if isEventSubset(receivedEvent, testEvent) {
			return true
		}
	}

	return false
}

// ForgetEvents erases all the recorded events.
func (mas *MockedAgentSink) ForgetEvents() {
	mas.mtx.Lock()
	defer mas.mtx.Unlock()

	mas.receivedEvents = nil
}

// Wait blocks until betweenEvents time has passed since the last received event, or up to max time has passed since the call.
// Returns false if we had to exhaust max.
func (mas *MockedAgentSink) Wait(betweenEvents, max time.Duration) bool {
	eventTimer := time.NewTimer(betweenEvents)
	maxTimer := time.NewTimer(max)

	for {
		select {
		// Reset betweenEvents timer whenever an event is received
		case <-mas.eventReceivedChan:
			if !eventTimer.Stop() {
				<-eventTimer.C
			}
			eventTimer.Reset(betweenEvents)

		// Return false if max timeout is reached
		case <-maxTimer.C:
			return false

		// Return true when small timeout is reached
		case <-eventTimer.C:
			return true
		}
	}
}

// isEventSubset checks whether the new event is a subset of the old event.
func isEventSubset(old, new *sdkEvent.Event) bool {
	if old == new {
		return true
	}

	if !strings.Contains(old.Category, new.Category) ||
		!strings.Contains(old.Summary, new.Summary) {
		return false
	}

	// Check new map is a subset of old map
	for nk, nv := range new.Attributes {
		// Check the old event contains all keys of the new one
		ov, found := old.Attributes[nk]
		if !found {
			return false
		}

		// Ensure types are equal.
		if reflect.TypeOf(ov) != reflect.TypeOf(nv) {
			return false
		}

		// If both are strings, check the old contains the new (partial matching).
		// Otherwise, just check for equality
		switch nvs := nv.(type) {
		case string:
			ovs := ov.(string)
			if !strings.Contains(ovs, nvs) {
				return false
			}
		default:
			if !reflect.DeepEqual(ov, nv) {
				return false
			}
		}

	}

	return true
}
