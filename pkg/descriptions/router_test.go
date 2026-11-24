package descriptions

import (
	"context"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/newrelic/nri-kube-events/pkg/common"
)

var (
	podsGR     = schema.GroupResource{Resource: "pods"}
	servicesGR = schema.GroupResource{Resource: "services"}
)

// fakeCoverage is a Coverage whose resources a test sets. Like objects.Coverage, it
// notifies through a channel buffered 1.
type fakeCoverage struct {
	covered map[schema.GroupResource]bool
	changed chan struct{}
	mu      sync.Mutex
}

func newFakeCoverage() *fakeCoverage {
	return &fakeCoverage{covered: map[schema.GroupResource]bool{}, changed: make(chan struct{}, 1)}
}

func (c *fakeCoverage) Covers(gr schema.GroupResource) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.covered[gr]
}

func (c *fakeCoverage) Changed() <-chan struct{} { return c.changed }

func (c *fakeCoverage) set(gr schema.GroupResource, on bool) {
	c.mu.Lock()
	c.covered[gr] = on
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// described is one HandleObject call: the object's name, verb and its "v" label.
type described struct {
	name, verb, version string
}

type recordingHandler struct {
	got []described
	mu  sync.Mutex
}

func (h *recordingHandler) HandleObject(o common.KubeObject) error {
	m, err := metaOf(o)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, described{name: m.GetName(), verb: o.Verb, version: m.GetLabels()["v"]})
	return nil
}

func metaOf(o common.KubeObject) (metav1.Object, error) {
	switch obj := o.Obj.(type) {
	case *v1.Pod:
		return obj, nil
	case *v1.Service:
		return obj, nil
	}
	return nil, assert.AnError
}

func (h *recordingHandler) take() []described {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.got
	h.got = nil
	return out
}

func (h *recordingHandler) has(want described) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.got {
		if d == want {
			return true
		}
	}
	return false
}

func pod(name, version string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"v": version}}}
}

func service(name string) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

func counterValue(t *testing.T, name string, gr schema.GroupResource) float64 {
	t.Helper()
	vec := descsSkippedTotal
	if name == "replayed" {
		vec = descsReplayedTotal
	}
	var m dto.Metric
	require.NoError(t, vec.WithLabelValues(gr.String()).Write(&m))
	return m.GetCounter().GetValue()
}

// startRouter starts pod and service informers over client and a Router over them, and
// returns the router, its handler and a function that stops both and waits for Run.
func startRouter(t *testing.T, client *fake.Clientset, coverage Coverage) (*Router, *recordingHandler, func()) {
	t.Helper()
	stop := make(chan struct{})
	factory := informers.NewSharedInformerFactory(client, 0)
	infs := []Informer{
		{SharedIndexInformer: factory.Core().V1().Pods().Informer(), Resource: podsGR},
		{SharedIndexInformer: factory.Core().V1().Services().Informer(), Resource: servicesGR},
	}
	h := &recordingHandler{}
	r := NewRouter(infs, map[string]ObjectHandler{"test": h}, coverage)
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(stop)
	}()
	var once sync.Once
	stopAll := func() {
		once.Do(func() {
			close(stop)
			<-done
			factory.Shutdown()
		})
	}
	t.Cleanup(stopAll)
	return r, h, stopAll
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, 5*time.Millisecond, msg)
}

func TestRouterWithoutCoverageDescribesEverything(t *testing.T) {
	client := fake.NewClientset(pod("p1", "1"), service("s1"))
	_, h, _ := startRouter(t, client, nil)
	eventually(t, func() bool {
		return h.has(described{"p1", "ADDED", "1"}) && h.has(described{"s1", "ADDED", ""})
	}, "both objects described")
}

// While pods are covered their changes are skipped. When coverage is withdrawn the router
// describes every cached pod once, in its current version, so a change made while covered
// is not lost. After that, changes are described again.
func TestRouterSkipsCoveredAndReplaysOnWithdrawal(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(pod("p1", "1"), service("s1"))
	cov := newFakeCoverage()
	_, h, _ := startRouter(t, client, cov)
	eventually(t, func() bool { return h.has(described{"p1", "ADDED", "1"}) }, "p1 described before coverage")
	h.take()

	skipped := counterValue(t, "skipped", podsGR)
	cov.set(podsGR, true)
	pods := client.CoreV1().Pods("default")
	_, err := pods.Update(ctx, pod("p1", "2"), metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = pods.Create(ctx, pod("p2", "1"), metav1.CreateOptions{})
	require.NoError(t, err)
	eventually(t, func() bool { return counterValue(t, "skipped", podsGR)-skipped == 2 }, "the pod update and add are skipped")

	_, err = client.CoreV1().Services("default").Create(ctx, service("s2"), metav1.CreateOptions{})
	require.NoError(t, err)
	eventually(t, func() bool { return h.has(described{"s2", "ADDED", ""}) }, "an uncovered resource is still described")
	for _, d := range h.take() {
		assert.NotContains(t, []string{"p1", "p2"}, d.name, "no pod described while covered")
	}

	replayed := counterValue(t, "replayed", podsGR)
	cov.set(podsGR, false)
	eventually(t, func() bool {
		return h.has(described{"p1", "UPDATE", "2"}) && h.has(described{"p2", "UPDATE", "1"})
	}, "every cached pod is described in its current version")
	assert.Equal(t, 2.0, counterValue(t, "replayed", podsGR)-replayed)

	_, err = pods.Update(ctx, pod("p2", "2"), metav1.UpdateOptions{})
	require.NoError(t, err)
	eventually(t, func() bool { return h.has(described{"p2", "UPDATE", "2"}) }, "changes are described after withdrawal")
}

// Coverage granted before Run starts applies from the first object, and replays nothing.
func TestRouterAppliesCoverageAtStart(t *testing.T) {
	client := fake.NewClientset(pod("p1", "1"), service("s1"))
	cov := newFakeCoverage()
	cov.set(podsGR, true)
	skipped := counterValue(t, "skipped", podsGR)
	_, h, _ := startRouter(t, client, cov)
	eventually(t, func() bool { return h.has(described{"s1", "ADDED", ""}) }, "s1 described")
	eventually(t, func() bool { return counterValue(t, "skipped", podsGR)-skipped == 1 }, "p1 skipped")
	for _, d := range h.take() {
		assert.NotEqual(t, "p1", d.name)
	}
}

// After the stop signal, withdrawn coverage replays nothing.
func TestRouterDoesNotReplayAfterStop(t *testing.T) {
	client := fake.NewClientset(pod("p1", "1"))
	cov := newFakeCoverage()
	cov.set(podsGR, true)
	skipped := counterValue(t, "skipped", podsGR)
	r, h, stopAll := startRouter(t, client, cov)
	eventually(t, func() bool { return counterValue(t, "skipped", podsGR)-skipped == 1 }, "p1 skipped")
	stopAll()

	cov.set(podsGR, false)
	stopped := make(chan struct{})
	close(stopped)
	r.applyCoverage(stopped)
	assert.Empty(t, h.take())
	assert.True(t, r.covered[podsGR], "the router's view is left as it was")
}
