package objects

import (
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
