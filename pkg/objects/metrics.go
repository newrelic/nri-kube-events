package objects

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const metricsPrefix = "nri_kube_events_objects_"

// The metrics are created unregistered, so they appear on /metrics only after
// RegisterMetrics. A build with the objects path off registers none of them.
var (
	metricRecordsSent = prometheus.NewCounter(prometheus.CounterOpts{
		Name: metricsPrefix + "records_sent_total",
		Help: "Records the infrastructure agent forwarder accepted with HTTP 204.",
	})
	metricBytesSent = prometheus.NewCounter(prometheus.CounterOpts{
		Name: metricsPrefix + "bytes_sent_total",
		Help: "Uncompressed JSON body bytes the infrastructure agent forwarder accepted with HTTP 204.",
	})
	metricSendFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: metricsPrefix + "send_failures_total",
		Help: "Object, tombstone and heartbeat sends that failed after retries. Sends cut short by shutdown are not counted.",
	})
	metricObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricsPrefix + "objects",
		Help: "Objects in the hash store per watched kind whose list succeeded, as of the last heartbeat.",
	}, []string{"kind"})
	metricHashStoreEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: metricsPrefix + "hash_store_entries",
		Help: "Entries in the in-memory hash store, as of the last load or flush.",
	})
	metricHashStoreLastFlush = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: metricsPrefix + "hash_store_last_flush_timestamp",
		Help: "Unix time in seconds of the last hash store flush that returned no error, including a flush with nothing to write.",
	})
)

// RegisterMetrics registers every objects metric with r. It returns the errors of the
// registrations that failed, joined.
func RegisterMetrics(r prometheus.Registerer) error {
	var errs []error
	for _, c := range []prometheus.Collector{
		metricRecordsSent,
		metricBytesSent,
		metricSendFailures,
		metricObjects,
		metricHashStoreEntries,
		metricHashStoreLastFlush,
	} {
		if err := r.Register(c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// objectsMetricMu serializes setObjectsMetric, so a reset and its sets are never interleaved
// with another call's.
var objectsMetricMu sync.Mutex

// setObjectsMetric replaces the objects gauge with counts, so a kind that left discovery or
// whose list now fails drops out.
func setObjectsMetric(counts map[string]int) {
	objectsMetricMu.Lock()
	defer objectsMetricMu.Unlock()
	metricObjects.Reset()
	for kind, n := range counts {
		metricObjects.WithLabelValues(kind).Set(float64(n))
	}
}
