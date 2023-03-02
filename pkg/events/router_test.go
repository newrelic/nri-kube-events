package events

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/newrelic/nri-kube-events/pkg/common"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestNewRouter(t *testing.T) {
	type args struct {
		informer *MockSharedIndexInformer
		sinks    map[string]Sink
	}
	tests := []struct {
		name   string
		args   args
		assert func(t *testing.T, args args, r *Router)
	}{
		{
			name: "AddEventHandler AddFunc",
			args: args{
				informer: new(MockSharedIndexInformer),
			},
			assert: func(t *testing.T, args args, r *Router) {
				assert.Len(t, args.informer.Calls, 1)
				hf := args.informer.Calls[0].Arguments.Get(0).(cache.ResourceEventHandlerFuncs)
				added := new(v1.Event)
				assert.NotNil(t, hf.AddFunc)
				go hf.AddFunc(added)
				select {
				case ke := <-r.workQueue:
					assert.NotNil(t, ke)
					assert.Equal(t, "ADDED", ke.Verb)
					assert.Equal(t, ke.Event, added)
					assert.Nil(t, ke.OldEvent)
				case <-time.After(1 * time.Second):
					assert.Fail(t, "Nothing on worker queue")
				}
			},
		},
		{
			name: "AddEventHandler UpdateFunc",
			args: args{
				informer: new(MockSharedIndexInformer),
			},
			assert: func(t *testing.T, args args, r *Router) {
				assert.Len(t, args.informer.Calls, 1)
				hf := args.informer.Calls[0].Arguments.Get(0).(cache.ResourceEventHandlerFuncs)
				oldObj := &v1.Event{
					Action: "Some old action",
				}
				newObj := &v1.Event{
					Action: "Some new action",
				}
				assert.NotNil(t, hf.UpdateFunc)
				go hf.UpdateFunc(oldObj, newObj)
				select {
				case ke := <-r.workQueue:
					assert.NotNil(t, ke)
					assert.Equal(t, "UPDATE", ke.Verb)
					assert.Equal(t, ke.Event, newObj)
					assert.Equal(t, ke.OldEvent, oldObj)
				case <-time.After(1 * time.Second):
					assert.Fail(t, "Nothing on worker queue")
				}
			},
		},
		{
			name: "workQueue",
			args: args{
				informer: new(MockSharedIndexInformer),
			},
			assert: func(t *testing.T, args args, r *Router) {
				assert.Equal(t, 1024, cap(r.workQueue), "Wrong default work queue length")
			},
		},
		{
			name: "sinks",
			args: args{
				informer: new(MockSharedIndexInformer),
				sinks: map[string]Sink{
					"stub": &stubSink{stubData: "some data"},
				},
			},
			assert: func(t *testing.T, args args, r *Router) {
				assert.Len(t, r.sinks, 1)
				s, ok := r.sinks["stub"]
				assert.True(t, ok)
				assert.NotNil(t, s)
				assert.Equal(t, args.sinks["stub"], s.(*observedSink).sink)
				obs := s.(*observedSink).observer
				assert.NotNil(t, obs)
				h := obs.(prometheus.Histogram)
				m := dto.Metric{}
				assert.NoError(t, h.Write(&m))
				name := "sink"
				value := "stub"
				// Check correct label pair added
				assert.Equal(t, []*dto.LabelPair{&dto.LabelPair{Name: &name, Value: &value}}, m.Label)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			tt.args.informer.
				On("AddEventHandler", mock.AnythingOfType("cache.ResourceEventHandlerFuncs")).
				Once()

			r := NewRouter(tt.args.informer, tt.args.sinks)
			assert.NotNil(t, r)
			tt.assert(t, tt.args, r)
			tt.args.informer.AssertExpectations(t)
		})
	}
}

type MockSharedIndexInformer struct {
	mock.Mock
	cache.SharedIndexInformer
}

func (m *MockSharedIndexInformer) SetupMock() {
	m.
		On("AddEventHandler", mock.AnythingOfType("cache.ResourceEventHandlerFuncs")).
		Once()
}

func (m *MockSharedIndexInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	m.Called(handler)
	return struct{}{}, nil
}

type stubSink struct {
	mock.Mock
	Sink
	stubData string
}

func (s *stubSink) HandleEvent(kubeEvent common.KubeEvent) error {
	args := s.Called(kubeEvent)
	return args.Error(0)
}

func TestRouter_Run(t *testing.T) {
	informer := new(MockSharedIndexInformer)
	informer.SetupMock()
	stubSink := new(stubSink)
	sinks := map[string]Sink{
		"stub": stubSink,
	}

	r := NewRouter(informer, sinks)
	stopChan := make(chan struct{})

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.Run(stopChan)
	}()

	ke := &v1.Event{
		Action: "Some old action",
	}

	stubSink.On("HandleEvent", mock.AnythingOfType("KubeEvent")).Run(func(args mock.Arguments) {
		log.Info("stub called")
		ake := args.Get(0).(common.KubeEvent)
		assert.Equal(t, ke, ake.Event)
		defer close(stopChan)
	}).Return(nil).Once()

	go func() {
		r.workQueue <- common.KubeEvent{
			Event: ke,
		}
	}()

	wg.Wait()
	stubSink.AssertExpectations(t)
}

func TestRouter_RunError(t *testing.T) {
	informer := new(MockSharedIndexInformer)
	informer.SetupMock()
	stubSink := new(stubSink)
	sinks := map[string]Sink{
		"stub": stubSink,
	}

	r := NewRouter(informer, sinks)
	stopChan := make(chan struct{})

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.Run(stopChan)
	}()

	ke := &v1.Event{
		Action: "Some old action",
	}

	expectedError := errors.New("something went wrong")
	stubSink.On("HandleEvent", mock.AnythingOfType("KubeEvent")).Run(func(args mock.Arguments) {
		defer close(stopChan)
	}).Return(expectedError).Once()

	go func() {
		r.workQueue <- common.KubeEvent{
			Event: ke,
		}
	}()

	wg.Wait()
	stubSink.AssertExpectations(t)
	c, err := eventsFailuresTotal.GetMetricWithLabelValues("stub")
	assert.NoError(t, err)
	m := dto.Metric{}
	assert.NoError(t, c.Write(&m))
	expCnt := float64(1)
	assert.Equal(t, expCnt, *m.Counter.Value)
}
