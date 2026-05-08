// Command ingest is the FlowScope flow ingestion service.
//
// It listens for NetFlow v5/v9, IPFIX, and sFlow v5 datagrams on UDP,
// decodes each into canonical record values, and emits them through
// the per-type Emitter fan-out points — Flow records to the in-process
// hot ring + the ClickHouse `flows` batcher; CounterSample records to
// the ClickHouse `iface_counter_samples` batcher.
//
// Today: NetFlow v5, NetFlow v9, IPFIX (v10), and sFlow v5 are wired.
// gNMI subscriptions are Phase 3. See VISION.md and CLAUDE.md for the
// full architecture.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/netflow"
	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/record"
	"github.com/dlambert-xbp/flowscope/internal/seqtrack"
	"github.com/dlambert-xbp/flowscope/internal/sflow"
	"github.com/dlambert-xbp/flowscope/internal/store"
)

const ringCapacity = 5000 // VISION.md §3.3

func main() {
	if err := run(); err != nil {
		slog.Error("ingest exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("FLOWSCOPE_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	netflowAddr := envOr("FLOWSCOPE_NETFLOW_ADDR", ":2055")
	sflowAddr := envOr("FLOWSCOPE_SFLOW_ADDR", ":6343")
	metricsAddr := envOr("FLOWSCOPE_METRICS_ADDR", ":9100")
	chDSN := envOr("FLOWSCOPE_CLICKHOUSE_DSN", "")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Spine: one ring + one flow Emitter + one counter Emitter for the
	// whole process. Persistence sinks register here.
	ring := record.NewRing(ringCapacity)
	flowSinks := []record.Sink{}
	counterSinks := []record.CounterSink{}

	var (
		flowBatcher    *store.FlowBatcher
		counterBatcher *store.CounterBatcher
		chConn         driver.Conn
	)
	if chDSN != "" {
		conn, err := store.Open(ctx, chDSN)
		if err != nil {
			return fmt.Errorf("clickhouse open: %w", err)
		}
		defer conn.Close()
		chConn = conn

		if err := store.Migrate(ctx, conn); err != nil {
			return fmt.Errorf("clickhouse migrate: %w", err)
		}
		slog.Info("clickhouse migrations applied")

		flowBatcher = store.NewFlowBatcher(conn, store.FlowBatcherOptions{
			MaxSize:     5000,
			MaxInterval: 200 * time.Millisecond,
		})
		flowSinks = append(flowSinks, flowBatcher)

		counterBatcher = store.NewCounterBatcher(conn, store.CounterBatcherOptions{
			MaxSize:     500,
			MaxInterval: time.Second,
		})
		counterSinks = append(counterSinks, counterBatcher)

		slog.Info("clickhouse batchers started",
			"flow_max_size", 5000, "flow_max_interval", "200ms",
			"counter_max_size", 500, "counter_max_interval", "1s",
		)
	} else {
		slog.Warn("FLOWSCOPE_CLICKHOUSE_DSN not set — running without persistence (ring only)")
	}

	flowEmitter := record.NewEmitter(ring, flowSinks...)
	counterEmitter := record.NewCounterEmitter(counterSinks...)
	templateCache := netflow.NewTemplateCache()
	tracker := seqtrack.New()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := runNetFlowListener(ctx, netflowAddr, flowEmitter, templateCache, tracker); err != nil {
			slog.Error("netflow listener exited", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := runSFlowListener(ctx, sflowAddr, flowEmitter, counterEmitter, tracker); err != nil {
			slog.Error("sflow listener exited", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := obs.ServeMetrics(ctx, metricsAddr); err != nil {
			slog.Error("metrics server exited", "err", err)
		}
	}()
	go ringSizeReporter(ctx, ring)
	if chConn != nil {
		go runExporterHealthFlusher(ctx, chConn, tracker)
	}

	slog.Info("flowscope ingest started",
		"netflow", netflowAddr,
		"sflow", sflowAddr,
		"metrics", metricsAddr,
		"ring_capacity", ringCapacity,
		"clickhouse", chDSN != "",
	)
	<-ctx.Done()
	slog.Info("shutting down")

	wg.Wait()
	if flowBatcher != nil {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := flowBatcher.Close(shutdownCtx); err != nil {
			slog.Warn("flow batcher close", "err", err)
		}
	}
	if counterBatcher != nil {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := counterBatcher.Close(shutdownCtx); err != nil {
			slog.Warn("counter batcher close", "err", err)
		}
	}
	return nil
}

// runNetFlowListener serves the NetFlow / IPFIX UDP port. Dispatches
// by version word: 5 → fixed v5 parser, 9 / 10 → template-driven
// v9 / IPFIX parser sharing the supplied TemplateCache.
func runNetFlowListener(ctx context.Context, addr string, emitter *record.Emitter, cache *netflow.TemplateCache, tracker *seqtrack.Tracker) error {
	conn, err := openUDP(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	buf := make([]byte, 65535)
	scratch := make([]record.Flow, 0, 32)

	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read udp: %w", err)
		}
		if n < 4 {
			continue
		}
		obs.UDPPacketsReceived.WithLabelValues("netflow").Inc()
		exporter := src.Addr().Unmap()
		version := binary.BigEndian.Uint16(buf[0:2])
		scratch = scratch[:0]
		var protocol string
		switch version {
		case 5:
			protocol = "netflow.v5"
			scratch, err = netflow.ParseV5(buf[:n], exporter, scratch)
		case 9:
			protocol = "netflow.v9"
			if seq, ok := netflow.ReadV9Sequence(buf[:n]); ok {
				tracker.Note(exporter, seqtrack.SourceNetFlowV9, seq)
			}
			scratch, err = netflow.ParseV9OrIPFIX(cache, buf[:n], exporter, scratch)
		case 10:
			protocol = "ipfix"
			scratch, err = netflow.ParseV9OrIPFIX(cache, buf[:n], exporter, scratch)
			// IPFIX seq increments by record count, not per
			// datagram (RFC 7011 §3.1). Call after parsing so we
			// have len(scratch) for an accurate expected-increment.
			// Skipped on parse failure — losing the seq for one bad
			// datagram is acceptable; the parse error counter
			// already records the failure.
			if err == nil {
				if seq, ok := netflow.ReadIPFIXSequence(buf[:n]); ok {
					tracker.NoteRecords(exporter, seqtrack.SourceIPFIX, seq, uint32(len(scratch)))
				}
			}
		default:
			obs.ParseErrorsTotal.WithLabelValues("netflow", "unknown_version").Inc()
			slog.Debug("unhandled netflow version", "version", version, "src", src)
			continue
		}
		if err != nil {
			obs.ParseErrorsTotal.WithLabelValues(protocol, parseErrReason(err)).Inc()
			slog.Debug("netflow parse error", "version", version, "err", err, "src", src)
			continue
		}
		obs.ParseRecordsTotal.WithLabelValues(protocol, "flow").Add(float64(len(scratch)))
		for _, f := range scratch {
			if err := emitter.Emit(ctx, f); err != nil {
				obs.EmitErrorsTotal.WithLabelValues("flow").Inc()
				slog.Warn("emit failed", "err", err)
			}
		}
	}
}

// runSFlowListener serves the sFlow v5 UDP port. Each datagram can
// contain both flow_samples and counters_samples; the parser produces
// both in one call and the listener routes each through its emitter.
func runSFlowListener(ctx context.Context, addr string, flowEmitter *record.Emitter, counterEmitter *record.CounterEmitter, tracker *seqtrack.Tracker) error {
	conn, err := openUDP(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	buf := make([]byte, 65535)
	flowScratch := make([]record.Flow, 0, 32)
	counterScratch := make([]record.CounterSample, 0, 8)

	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read udp: %w", err)
		}
		if n < 28 {
			continue
		}
		obs.UDPPacketsReceived.WithLabelValues("sflow").Inc()
		fallback := src.Addr().Unmap()
		// Note datagram seq before parse; agent_address override (per
		// VISION.md §4.1) means the post-parse exporter may differ from
		// the UDP source. Use the UDP source for tracking — parsing-time
		// agent address is best-effort and may be 0.0.0.0.
		if seq, ok := sflow.ReadSequence(buf[:n]); ok {
			tracker.Note(fallback, seqtrack.SourceSFlow, seq)
		}
		flowScratch = flowScratch[:0]
		counterScratch = counterScratch[:0]
		flowScratch, counterScratch, err = sflow.Parse(buf[:n], fallback, flowScratch, counterScratch)
		if err != nil {
			obs.ParseErrorsTotal.WithLabelValues("sflow.v5", parseErrReason(err)).Inc()
			slog.Debug("sflow parse error", "err", err, "src", src)
			continue
		}
		if len(flowScratch) > 0 {
			obs.ParseRecordsTotal.WithLabelValues("sflow.v5", "flow").Add(float64(len(flowScratch)))
		}
		if len(counterScratch) > 0 {
			obs.ParseRecordsTotal.WithLabelValues("sflow.v5", "counter").Add(float64(len(counterScratch)))
		}
		for _, f := range flowScratch {
			if err := flowEmitter.Emit(ctx, f); err != nil {
				obs.EmitErrorsTotal.WithLabelValues("flow").Inc()
				slog.Warn("flow emit failed", "err", err)
			}
		}
		for _, c := range counterScratch {
			if err := counterEmitter.Emit(ctx, c); err != nil {
				obs.EmitErrorsTotal.WithLabelValues("counter").Inc()
				slog.Warn("counter emit failed", "err", err)
			}
		}
	}
}

// ringSizeReporter periodically samples the hot ring size and updates
// the obs.RingSize gauge. Ring.Push is on the hot path and shouldn't
// pay for a metrics call per record; sampling at 1 Hz is plenty for
// the dashboard.
func ringSizeReporter(ctx context.Context, ring *record.Ring) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			obs.RingSize.Set(float64(ring.Len()))
		}
	}
}

// runExporterHealthFlusher drains the seqtrack tracker every 10s
// and writes accumulated per-(exporter, source) datagram and gap
// counts to the exporter_health table. The api service queries
// this table to render the per-exporter accuracy panel on Overview.
//
// Skips writes when the snapshot is empty so silent exporters
// don't bloat the table.
func runExporterHealthFlusher(ctx context.Context, conn driver.Conn, tracker *seqtrack.Tracker) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snapshots := tracker.Drain()
			if len(snapshots) == 0 {
				continue
			}
			if err := flushExporterHealth(ctx, conn, snapshots); err != nil {
				slog.Warn("exporter health flush failed", "err", err, "rows", len(snapshots))
			}
		}
	}
}

func flushExporterHealth(ctx context.Context, conn driver.Conn, snapshots []seqtrack.Snapshot) error {
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO exporter_health (ts, exporter, source, datagrams, seq_gaps, last_seq)")
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	now := time.Now().UTC()
	for _, s := range snapshots {
		exp := s.Exporter
		if !exp.Is6() {
			exp = netip.AddrFrom16(exp.As16())
		}
		if err := batch.Append(now, exp.As16(), s.Source, s.Datagrams, s.SeqGaps, s.LastSeq); err != nil {
			return fmt.Errorf("append: %w", err)
		}
	}
	return batch.Send()
}

// parseErrReason classifies parse errors into stable Prometheus label
// values. Unknown errors fall back to "other" so the cardinality stays
// bounded.
func parseErrReason(err error) string {
	switch {
	case errors.Is(err, netflow.ErrShortPacket), errors.Is(err, sflow.ErrShortPacket):
		return "short_packet"
	case errors.Is(err, netflow.ErrBadVersion), errors.Is(err, sflow.ErrBadVersion):
		return "bad_version"
	case errors.Is(err, netflow.ErrBadCount):
		return "bad_count"
	case errors.Is(err, netflow.ErrTruncated), errors.Is(err, sflow.ErrTruncated):
		return "truncated"
	case errors.Is(err, netflow.ErrV9BadHeader):
		return "bad_header"
	case errors.Is(err, netflow.ErrV9BadFlowsetLen):
		return "bad_flowset_len"
	case errors.Is(err, sflow.ErrAddressKind):
		return "bad_agent_addr"
	default:
		return "other"
	}
}

// openUDP resolves and binds a UDP listener that closes when ctx is
// cancelled.
func openUDP(ctx context.Context, addr string) (*net.UDPConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return conn, nil
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
