// Command ingest is the FlowScope flow ingestion service.
//
// It listens for NetFlow v5/v9, IPFIX, and sFlow v5 datagrams on UDP,
// decodes each into canonical record values, and emits them through
// the per-type Emitter fan-out points — Flow records to the in-process
// hot ring + the ClickHouse `flows` batcher; CounterSample records to
// the ClickHouse `iface_counter_samples` batcher.
//
// Today: NetFlow v5 and sFlow v5 are wired. NetFlow v9 / IPFIX land
// in a follow-up slice. See VISION.md and CLAUDE.md for the full
// architecture.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/netflow"
	"github.com/dlambert-xbp/flowscope/internal/record"
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
	)
	if chDSN != "" {
		conn, err := store.Open(ctx, chDSN)
		if err != nil {
			return fmt.Errorf("clickhouse open: %w", err)
		}
		defer conn.Close()

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

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := runNetFlowListener(ctx, netflowAddr, flowEmitter); err != nil {
			slog.Error("netflow listener exited", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := runSFlowListener(ctx, sflowAddr, flowEmitter, counterEmitter); err != nil {
			slog.Error("sflow listener exited", "err", err)
		}
	}()

	slog.Info("flowscope ingest started",
		"netflow", netflowAddr,
		"sflow", sflowAddr,
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

// runNetFlowListener serves the NetFlow / IPFIX UDP port. Today only
// v5 is wired; v9 and IPFIX dispatch lands when those parsers ship.
func runNetFlowListener(ctx context.Context, addr string, emitter *record.Emitter) error {
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
		exporter := src.Addr().Unmap()
		version := binary.BigEndian.Uint16(buf[0:2])
		switch version {
		case 5:
			scratch = scratch[:0]
			scratch, err = netflow.ParseV5(buf[:n], exporter, scratch)
			if err != nil {
				slog.Debug("netflow v5 parse error", "err", err, "src", src)
				continue
			}
			for _, f := range scratch {
				if err := emitter.Emit(ctx, f); err != nil {
					slog.Warn("emit failed", "err", err)
				}
			}
		default:
			slog.Debug("unhandled netflow version", "version", version, "src", src)
		}
	}
}

// runSFlowListener serves the sFlow v5 UDP port. Each datagram can
// contain both flow_samples and counters_samples; the parser produces
// both in one call and the listener routes each through its emitter.
func runSFlowListener(ctx context.Context, addr string, flowEmitter *record.Emitter, counterEmitter *record.CounterEmitter) error {
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
		fallback := src.Addr().Unmap()
		flowScratch = flowScratch[:0]
		counterScratch = counterScratch[:0]
		flowScratch, counterScratch, err = sflow.Parse(buf[:n], fallback, flowScratch, counterScratch)
		if err != nil {
			slog.Debug("sflow parse error", "err", err, "src", src)
			continue
		}
		for _, f := range flowScratch {
			if err := flowEmitter.Emit(ctx, f); err != nil {
				slog.Warn("flow emit failed", "err", err)
			}
		}
		for _, c := range counterScratch {
			if err := counterEmitter.Emit(ctx, c); err != nil {
				slog.Warn("counter emit failed", "err", err)
			}
		}
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
