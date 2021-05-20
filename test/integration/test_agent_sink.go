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

// TestAgentSink is an instrumented infra-agent sink for testing e2e reception and processing.
type TestAgentSink struct {
	agentSink         events.Sink
	httpServer        *httptest.Server
	eventReceivedChan chan struct{}
	receivedEvents    []sdkEvent.Event
	mtx               *sync.RWMutex
}

// NewTestAgentSink returns an instrumented infra-agent sink for testing.
func NewTestAgentSink() *TestAgentSink {
	mockedAgentSink := &TestAgentSink{
		mtx:               &sync.RWMutex{},
		eventReceivedChan: make(chan struct{}, 128),
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
func (tas *TestAgentSink) HandleEvent(kubeEvent events.KubeEvent) error {
	tas.eventReceivedChan <- struct{}{}
	return tas.agentSink.HandleEvent(kubeEvent)
}

// ServeHTTP handles a request that would be for the infra-agent and stores the unmarshalled event.
func (tas *TestAgentSink) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	tas.mtx.Lock()
	defer tas.mtx.Unlock()

	var ev struct {
		Data []struct {
			Events []sdkEvent.Event `json:"events"`
		} `json:"data"`
	}

	defer r.Body.Close() // nolint:errcheck
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Fatalf("error reading request body: %v", err)
	}

	err = json.Unmarshal(body, &ev)
	if err != nil {
		log.Fatalf("error unmarshalling request body: %v", err)
	}

	if len(ev.Data) == 0 {
		log.Warnf("received payload with no data: %s", string(body))
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	tas.receivedEvents = append(tas.receivedEvents, ev.Data[0].Events...)
	rw.WriteHeader(http.StatusNoContent) // Return 204 as the infra-agent does.
}

// Has relaxedly checks whether the mocked agent has received an event.
func (tas *TestAgentSink) Has(testEvent *sdkEvent.Event) bool {
	tas.mtx.RLock()
	defer tas.mtx.RUnlock()

	for i := range tas.receivedEvents {
		receivedEvent := &tas.receivedEvents[i]

		if isEventSubset(receivedEvent, testEvent) {
			return true
		}
	}

	return false
}

// Events returns the list of events the mock has captured.
func (tas *TestAgentSink) Events() []sdkEvent.Event {
	tas.mtx.RLock()
	defer tas.mtx.RUnlock()

	retEvents := make([]sdkEvent.Event, len(tas.receivedEvents))
	copy(retEvents, tas.receivedEvents)

	return retEvents
}

// ForgetEvents erases all the recorded events.
func (tas *TestAgentSink) ForgetEvents() {
	tas.mtx.Lock()
	defer tas.mtx.Unlock()

	tas.receivedEvents = nil
}

// Wait blocks until betweenEvents time has passed since the last received event, or up to max time has passed since the call.
// Returns false if we had to exhaust max.
func (tas *TestAgentSink) Wait(betweenEvents, max time.Duration) bool {
	eventTimer := time.NewTimer(betweenEvents)
	maxTimer := time.NewTimer(max)

	for {
		select {
		// Reset betweenEvents timer whenever an event is received.
		case <-tas.eventReceivedChan:
			if !eventTimer.Stop() {
				<-eventTimer.C
			}
			eventTimer.Reset(betweenEvents)

		// Return false if max timeout is reached.
		case <-maxTimer.C:
			return false

		// Return true when small timeout is reached.
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

	// Check new map is a subset of old map.
	for nk, nv := range new.Attributes {
		// Check the old event contains all keys of the new one.
		ov, found := old.Attributes[nk]
		if !found {
			return false
		}

		// Ensure types are equal.
		if reflect.TypeOf(ov) != reflect.TypeOf(nv) {
			return false
		}

		// If both are strings, check the old contains the new (partial matching).
		// Otherwise, just check for equality.
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
