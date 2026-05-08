package obs

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// IngestHealth is the JSON snapshot exposed by cmd/ingest at
// /health/ingest. It collapses the relevant Prometheus counters
// into a single typed payload the api service can fetch and the
// Overview tab can render. Stable schema — adding new counters
// here should be additive.
type IngestHealth struct {
	StartedAt     time.Time           `json:"started_at"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	UDPReceived   map[string]uint64   `json:"udp_received_total"`
	ParseRecords  []IngestPair        `json:"parse_records_total"`
	ParseErrors   []IngestPair        `json:"parse_errors_total"`
	EmitErrors    map[string]uint64   `json:"emit_errors_total"`
	TemplateCache IngestTemplateCache `json:"template_cache"`
	RingSize      float64             `json:"ring_size"`
}

// IngestPair is one row in a per-pair counter dump. Sorted
// lexicographically for stable JSON output so dashboards can diff
// snapshots cleanly.
type IngestPair struct {
	Protocol string `json:"protocol"`
	Label    string `json:"label"` // reason for errors, kind for records
	Value    uint64 `json:"value"`
}

type IngestTemplateCache struct {
	Hits   uint64  `json:"hits"`
	Misses uint64  `json:"misses"`
	Size   float64 `json:"size"`
}

// startedAtUnixNano is captured the first time ServeHealth is
// invoked. SnapshotIngestHealth uses it for the uptime field.
var startedAtUnixNano int64

// SnapshotIngestHealth gathers current counter values into the
// IngestHealth shape. Reads are point-in-time and may not all
// reflect the same instant — the Overview panel that consumes
// this is happy with within-a-tick consistency.
func SnapshotIngestHealth() IngestHealth {
	startedNs := atomic.LoadInt64(&startedAtUnixNano)
	var started time.Time
	if startedNs == 0 {
		started = time.Now()
	} else {
		started = time.Unix(0, startedNs)
	}
	return IngestHealth{
		StartedAt:     started,
		UptimeSeconds: int64(time.Since(started).Seconds()),
		UDPReceived:   counterVecToMap(UDPPacketsReceived, "listener"),
		ParseRecords:  counterVecToPairs(ParseRecordsTotal, "protocol", "kind"),
		ParseErrors:   counterVecToPairs(ParseErrorsTotal, "protocol", "reason"),
		EmitErrors:    counterVecToMap(EmitErrorsTotal, "emitter"),
		TemplateCache: IngestTemplateCache{
			Hits:   counterValue(TemplateCacheHits),
			Misses: counterValue(TemplateCacheMisses),
			Size:   gaugeValue(TemplateCacheSize),
		},
		RingSize: gaugeValue(RingSize),
	}
}

// ServeHealth returns the http.Handler for the JSON ingest health
// endpoint. Mounted by ServeMetrics on cmd/ingest.
func ServeHealth() http.Handler {
	atomic.CompareAndSwapInt64(&startedAtUnixNano, 0, time.Now().UnixNano())
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SnapshotIngestHealth())
	})
}

func counterValue(c prometheus.Counter) uint64 {
	if c == nil {
		return 0
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	return uint64(m.GetCounter().GetValue())
}

func gaugeValue(g prometheus.Gauge) float64 {
	if g == nil {
		return 0
	}
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func counterVecToMap(v *prometheus.CounterVec, labelKey string) map[string]uint64 {
	out := map[string]uint64{}
	collect := make(chan prometheus.Metric, 32)
	go func() {
		v.Collect(collect)
		close(collect)
	}()
	for m := range collect {
		var d dto.Metric
		if err := m.Write(&d); err != nil {
			continue
		}
		for _, lp := range d.GetLabel() {
			if lp.GetName() == labelKey {
				out[lp.GetValue()] = uint64(d.GetCounter().GetValue())
				break
			}
		}
	}
	return out
}

func counterVecToPairs(v *prometheus.CounterVec, keyA, keyB string) []IngestPair {
	out := []IngestPair{}
	collect := make(chan prometheus.Metric, 32)
	go func() {
		v.Collect(collect)
		close(collect)
	}()
	for m := range collect {
		var d dto.Metric
		if err := m.Write(&d); err != nil {
			continue
		}
		var a, b string
		for _, lp := range d.GetLabel() {
			switch lp.GetName() {
			case keyA:
				a = lp.GetValue()
			case keyB:
				b = lp.GetValue()
			}
		}
		out = append(out, IngestPair{
			Protocol: a,
			Label:    b,
			Value:    uint64(d.GetCounter().GetValue()),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Protocol != out[j].Protocol {
			return out[i].Protocol < out[j].Protocol
		}
		return out[i].Label < out[j].Label
	})
	return out
}
