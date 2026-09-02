package service

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a dedicated Prometheus registry (not the global default
// registry), so multiple Services in one process — as in tests — never
// fight over duplicate registrations.
type Metrics struct {
	registry               *prometheus.Registry
	mountOperationsTotal   *prometheus.CounterVec
	mountOperationDuration *prometheus.HistogramVec
	childCrashesTotal      prometheus.Counter
	reconcileErrorsTotal   prometheus.Counter
	// logDroppedLinesTotal is reserved for the SSE log-streaming endpoint
	// (design.md), which is not implemented yet; it stays registered, and
	// at zero, so its name is stable once that endpoint exists.
	logDroppedLinesTotal prometheus.Counter
}

// NewMetrics creates and registers irodsfsd's Prometheus metrics, including
// a live collector for irodsfsd_mounts backed by manager's current state.
func NewMetrics(manager *MountManager) *Metrics {
	metrics := &Metrics{
		registry: prometheus.NewRegistry(),
		mountOperationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "irodsfsd_mount_operations_total",
			Help: "Total mount and unmount API requests, by operation and result.",
		}, []string{"operation", "result"}),
		mountOperationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "irodsfsd_mount_operation_duration_seconds",
			Help:    "Duration of the synchronous Mount/Unmount API call, by operation.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation"}),
		childCrashesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "irodsfsd_child_crashes_total",
			Help: "Total number of unexpected mount child process exits.",
		}),
		reconcileErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "irodsfsd_reconcile_errors_total",
			Help: "Total number of errors encountered while reconciling stored mount intent against real system state.",
		}),
		logDroppedLinesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "irodsfsd_log_dropped_lines_total",
			Help: "Total number of live log lines dropped by a slow log-stream client.",
		}),
	}
	metrics.registry.MustRegister(
		metrics.mountOperationsTotal,
		metrics.mountOperationDuration,
		metrics.childCrashesTotal,
		metrics.reconcileErrorsTotal,
		metrics.logDroppedLinesTotal,
		newMountsCollector(manager),
	)
	return metrics
}

// Handler returns the http.Handler that serves this registry's metrics in
// the Prometheus exposition format.
func (metrics *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{})
}

func (metrics *Metrics) observeMountOperation(operation string, result string, duration time.Duration) {
	metrics.mountOperationsTotal.WithLabelValues(operation, result).Inc()
	metrics.mountOperationDuration.WithLabelValues(operation).Observe(duration.Seconds())
}

func (metrics *Metrics) recordChildCrash() {
	metrics.childCrashesTotal.Inc()
}

func (metrics *Metrics) recordReconcileError() {
	metrics.reconcileErrorsTotal.Inc()
}

// mountsCollector reports irodsfsd_mounts{state=...} by reading manager's
// current in-memory state on every scrape, rather than requiring every
// state-transition call site to keep a separate gauge up to date.
type mountsCollector struct {
	manager *MountManager
	desc    *prometheus.Desc
}

func newMountsCollector(manager *MountManager) *mountsCollector {
	return &mountsCollector{
		manager: manager,
		desc: prometheus.NewDesc(
			"irodsfsd_mounts",
			"Current number of managed mounts, by state.",
			[]string{"state"},
			nil,
		),
	}
}

func (collector *mountsCollector) Describe(descs chan<- *prometheus.Desc) {
	descs <- collector.desc
}

func (collector *mountsCollector) Collect(metrics chan<- prometheus.Metric) {
	mounts, err := collector.manager.ListMounts(context.Background(), nil)
	if err != nil {
		return
	}
	counts := make(map[string]int)
	for _, mount := range mounts {
		counts[mount.GetState().String()]++
	}
	for state, count := range counts {
		metrics <- prometheus.MustNewConstMetric(collector.desc, prometheus.GaugeValue, float64(count), state)
	}
}
