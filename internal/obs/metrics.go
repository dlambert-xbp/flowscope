// Package obs centralises FlowScope observability — Prometheus
// metrics, structured logging helpers, and the small HTTP server that
// exposes /metrics on services that don't already run an HTTP stack
// (today: cmd/ingest).
//
// Every counter visible on the Overview dashboard's "Receiver health"
// tile must originate here. Adding a new failure mode means adding the
// counter in the same PR — see VISION.md §11 #6.
package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Listener / receiver metrics.
var (
	// UDPPacketsReceived counts datagrams accepted from the kernel
	// per listener. Drops at the kernel buffer level are not yet
	// counted — that requires reading /proc/net/snmp UdpInErrors
	// or SO_RXQ_OVFL, both Linux-only. TODO.
	UDPPacketsReceived = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_udp_packets_received_total",
			Help: "Total UDP datagrams received per listener.",
		},
		[]string{"listener"},
	)

	// ParseRecordsTotal counts records emitted by parsers, by
	// protocol (netflow.v5 / netflow.v9 / ipfix / sflow.v5) and
	// kind (flow / counter).
	ParseRecordsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_parse_records_total",
			Help: "Total records produced by parsers, by protocol and kind.",
		},
		[]string{"protocol", "kind"},
	)

	// ParseErrorsTotal counts parse failures. Reasons are short
	// stable identifiers (short_packet, bad_version, truncated,
	// template_miss, ...).
	ParseErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_parse_errors_total",
			Help: "Total parse errors by protocol and reason.",
		},
		[]string{"protocol", "reason"},
	)

	// IngestDroppedUnauthorized counts UDP datagrams dropped at the
	// listener because the source IP is not on the exporter
	// allowlist (or its row is disabled). Labeled by exporter so the
	// Overview "Receiver health" panel can highlight which sources
	// are being rejected. Empty allowlist = accept-all and never
	// increments this counter.
	//
	// Cardinality note: a hostile sender can spray packets from
	// random source IPs and bloat this label set. Operators running
	// in deny-by-default mode should fronted the listener with a UDP
	// firewall rule that limits sources to the management subnet —
	// the same rule that would normally precede an allowlist anyway.
	IngestDroppedUnauthorized = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_ingest_dropped_unauthorized_total",
			Help: "UDP datagrams dropped at the listener because the source is not on the exporter allowlist (or is disabled).",
		},
		[]string{"exporter"},
	)
)

// NetFlow v9 / IPFIX template cache metrics.
var (
	TemplateCacheHits = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "flowscope_template_cache_hits_total",
			Help: "Total NetFlow v9 / IPFIX template lookup hits (data flowsets decoded).",
		},
	)

	TemplateCacheMisses = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "flowscope_template_cache_misses_total",
			Help: "Total NetFlow v9 / IPFIX template lookup misses (data flowsets dropped).",
		},
	)

	TemplateCacheSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "flowscope_template_cache_size",
			Help: "Number of templates currently cached.",
		},
	)
)

// Storage / batcher metrics.
var (
	BatchFlushesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_batch_flushes_total",
			Help: "Total batch flushes by table and result (ok / error).",
		},
		[]string{"table", "result"},
	)

	BatchRowsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_batch_rows_total",
			Help: "Total rows successfully written to ClickHouse, by table.",
		},
		[]string{"table"},
	)

	BatchFlushDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "flowscope_batch_flush_duration_seconds",
			Help:    "Latency of batch flush to ClickHouse, by table.",
			Buckets: prometheus.ExponentialBucketsRange(0.001, 5, 12),
		},
		[]string{"table"},
	)
)

// Hot ring metrics.
var (
	RingSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "flowscope_ring_size",
			Help: "Current number of records in the in-process flow ring.",
		},
	)
)

// Emit fan-out metrics.
var (
	EmitErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_emit_errors_total",
			Help: "Errors returned by sinks during Emit fan-out, by emitter type.",
		},
		[]string{"emitter"},
	)
)

// Webhook dispatcher metrics. Labeled by kind ('slack' | 'teams' |
// 'pagerduty' | 'http') so the Overview panel can show which channel
// type is silent / flapping. result is 'ok' for production sends and
// 'test' for the /webhooks/{id}/test endpoint.
var (
	WebhookDispatchTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_webhook_dispatch_total",
			Help: "Successful webhook POSTs by kind and result.",
		},
		[]string{"kind", "result"},
	)

	WebhookDispatchFailuresTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_webhook_dispatch_failures_total",
			Help: "Webhook dispatch failures by kind and reason ('format', 'decrypt', 'client_error', 'exhausted', 'test_network', 'test_status', 'no_crypter').",
		},
		[]string{"kind", "reason"},
	)

	WebhookDispatchRetriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flowscope_webhook_dispatch_retries_total",
			Help: "Webhook dispatch retry attempts (every non-final failed attempt increments this).",
		},
		[]string{"kind"},
	)
)

// Alert leader-election metrics. The lease is ClickHouse-backed (see
// migration 000010_leader_lease.sql). These counters are observed by
// the Overview "Receiver health" panel so an operator can spot a
// flapping lease before users notice silent alerts.
var (
	AlertLeaseAcquiredTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "flowscope_alert_lease_acquired_total",
			Help: "Times this replica won the alert leader lease.",
		},
	)

	AlertLeaseLostTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "flowscope_alert_lease_lost_total",
			Help: "Times this replica lost the alert leader lease (DB reports a different holder).",
		},
	)

	AlertLeaseRenewFailedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "flowscope_alert_lease_renew_failed_total",
			Help: "Failed lease renew attempts (DB error or lease expired before write). A non-zero rate is a red flag.",
		},
	)

	AlertLeaseHeld = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "flowscope_alert_lease_held",
			Help: "1 if this replica currently holds the alert leader lease, 0 otherwise.",
		},
	)
)
