package metrics

import (
	"github.com/grafana/crocochrome/internal/chromium"
	"github.com/prometheus/client_golang/prometheus"
)

// HeapSnapshotSource reports the latest heap snapshot and session labels from the supervisor.
// Returning ok=false means there is no session or no snapshot yet; the collector emits nothing in that case.
type HeapSnapshotSource interface {
	ActiveHeapSnapshot() (*chromium.HeapSnapshot, map[string]string, bool)
}

// heapLabels are the Prometheus label names, in the order they're supplied to MustNewConstMetric.
// The mapping from crocochrome's allowlisted metadata keys is: id→check_id, tenantID→tenant_id, regionID→region_id.
var heapLabels = []string{"check_id", "tenant_id", "region_id"}

// HeapCollector is a prometheus.Collector that emits V8/DOM metrics from the active chromium session.
// Values are read at scrape time from the supervisor — no intermediate gauge state, so metrics disappear
// the moment a session ends rather than leaking stale labels into the registry.
type HeapCollector struct {
	src HeapSnapshotSource

	heapUsed      *prometheus.Desc
	heapTotal     *prometheus.Desc
	heapLimit     *prometheus.Desc
	embedderHeap  *prometheus.Desc
	backingStore  *prometheus.Desc
	domNodes      *prometheus.Desc
	domDocuments  *prometheus.Desc
	domListeners  *prometheus.Desc
	probeLastSucc *prometheus.Desc
}

// NewHeapCollector builds a HeapCollector and registers it with reg.
func NewHeapCollector(reg prometheus.Registerer, src HeapSnapshotSource) *HeapCollector {
	c := &HeapCollector{
		src: src,
		heapUsed: prometheus.NewDesc(
			"sm_crocochrome_v8_heap_used_bytes",
			"Live object size on the V8 JS heap, summed across page renderers for the active session.",
			heapLabels, nil,
		),
		heapTotal: prometheus.NewDesc(
			"sm_crocochrome_v8_heap_total_bytes",
			"Reserved size of the V8 JS heap, summed across page renderers for the active session.",
			heapLabels, nil,
		),
		heapLimit: prometheus.NewDesc(
			"sm_crocochrome_v8_heap_limit_bytes",
			"V8-auto-detected JS heap ceiling (performance.memory.jsHeapSizeLimit). Shared across renderers.",
			heapLabels, nil,
		),
		embedderHeap: prometheus.NewDesc(
			"sm_crocochrome_v8_embedder_heap_bytes",
			"Oilpan (Blink C++) GC heap usage, summed across page renderers for the active session.",
			heapLabels, nil,
		),
		backingStore: prometheus.NewDesc(
			"sm_crocochrome_v8_backing_store_bytes",
			"ArrayBuffer and external string storage, summed across page renderers for the active session.",
			heapLabels, nil,
		),
		domNodes: prometheus.NewDesc(
			"sm_crocochrome_dom_nodes",
			"Live DOM node count, summed across page targets for the active session.",
			heapLabels, nil,
		),
		domDocuments: prometheus.NewDesc(
			"sm_crocochrome_dom_documents",
			"Live Document count, summed across page targets for the active session.",
			heapLabels, nil,
		),
		domListeners: prometheus.NewDesc(
			"sm_crocochrome_dom_event_listeners",
			"Live JS event listener count, summed across page targets for the active session.",
			heapLabels, nil,
		),
		probeLastSucc: prometheus.NewDesc(
			"sm_crocochrome_heap_probe_last_success_timestamp_seconds",
			"Unix timestamp of the most recent successful heap probe for the active session. Use to detect stale data.",
			heapLabels, nil,
		),
	}

	reg.MustRegister(c)
	return c
}

func (c *HeapCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.heapUsed
	ch <- c.heapTotal
	ch <- c.heapLimit
	ch <- c.embedderHeap
	ch <- c.backingStore
	ch <- c.domNodes
	ch <- c.domDocuments
	ch <- c.domListeners
	ch <- c.probeLastSucc
}

func (c *HeapCollector) Collect(ch chan<- prometheus.Metric) {
	snap, labels, ok := c.src.ActiveHeapSnapshot()
	if !ok {
		return
	}

	// Map crocochrome's allowlisted metadata keys to Prometheus label values, in the order heapLabels declares.
	lv := []string{labels["id"], labels["tenantID"], labels["regionID"]}

	ch <- prometheus.MustNewConstMetric(c.heapUsed, prometheus.GaugeValue, float64(snap.V8HeapUsedBytes), lv...)
	ch <- prometheus.MustNewConstMetric(c.heapTotal, prometheus.GaugeValue, float64(snap.V8HeapTotalBytes), lv...)
	ch <- prometheus.MustNewConstMetric(c.heapLimit, prometheus.GaugeValue, float64(snap.V8HeapLimitBytes), lv...)
	ch <- prometheus.MustNewConstMetric(c.embedderHeap, prometheus.GaugeValue, float64(snap.V8EmbedderHeapBytes), lv...)
	ch <- prometheus.MustNewConstMetric(c.backingStore, prometheus.GaugeValue, float64(snap.V8BackingStoreBytes), lv...)
	ch <- prometheus.MustNewConstMetric(c.domNodes, prometheus.GaugeValue, float64(snap.DOMNodes), lv...)
	ch <- prometheus.MustNewConstMetric(c.domDocuments, prometheus.GaugeValue, float64(snap.DOMDocuments), lv...)
	ch <- prometheus.MustNewConstMetric(c.domListeners, prometheus.GaugeValue, float64(snap.DOMEventListeners), lv...)
	ch <- prometheus.MustNewConstMetric(c.probeLastSucc, prometheus.GaugeValue, float64(snap.CollectedAt.Unix()), lv...)
}
