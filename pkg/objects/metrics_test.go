package objects

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetObjectsMetricDropsGoneKinds(t *testing.T) {
	setObjectsMetric(map[string]int{"Pod": 3, "Gone": 1})
	setObjectsMetric(map[string]int{"Pod": 4})

	ch := make(chan prometheus.Metric, 10)
	metricObjects.Collect(ch)
	close(ch)
	got := map[string]float64{}
	for m := range ch {
		var d dto.Metric
		require.NoError(t, m.Write(&d))
		got[d.GetLabel()[0].GetValue()] = d.GetGauge().GetValue()
	}
	assert.Equal(t, map[string]float64{"Pod": 4}, got)
}

func gatheredObjectsFamilies(t *testing.T, g prometheus.Gatherer) []string {
	t.Helper()
	mfs, err := g.Gather()
	require.NoError(t, err)
	var names []string
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), metricsPrefix) {
			names = append(names, mf.GetName())
		}
	}
	return names
}

func TestRegisterMetricsOnlyWhenCalled(t *testing.T) {
	// Nothing in the package registers globally: the default registry gathers no objects
	// family, and each objects collector can still be registered there. A vector with no
	// children gathers nothing, so the Register check is what catches a registered vector.
	assert.Empty(t, gatheredObjectsFamilies(t, prometheus.DefaultGatherer))
	for _, c := range objectsCollectors() {
		err := prometheus.DefaultRegisterer.Register(c)
		require.NoError(t, err, "an objects metric is registered globally without RegisterMetrics")
		prometheus.DefaultRegisterer.Unregister(c)
	}

	r := prometheus.NewRegistry()
	require.NoError(t, RegisterMetrics(r))
	assert.Subset(t, gatheredObjectsFamilies(t, r), []string{
		"nri_kube_events_objects_bytes_sent_total",
		"nri_kube_events_objects_hash_store_entries",
		"nri_kube_events_objects_hash_store_last_flush_timestamp",
		"nri_kube_events_objects_records_sent_total",
		"nri_kube_events_objects_resends_total",
		"nri_kube_events_objects_send_failures_total",
	})

	setObjectsMetric(map[string]int{"Pod": 1})
	metricForwarderRejections.WithLabelValues("500").Add(0)
	assert.ElementsMatch(t, []string{
		"nri_kube_events_objects_bytes_sent_total",
		"nri_kube_events_objects_forwarder_rejections_total",
		"nri_kube_events_objects_hash_store_entries",
		"nri_kube_events_objects_hash_store_last_flush_timestamp",
		"nri_kube_events_objects_objects",
		"nri_kube_events_objects_records_sent_total",
		"nri_kube_events_objects_resends_total",
		"nri_kube_events_objects_send_failures_total",
	}, gatheredObjectsFamilies(t, r))
}
