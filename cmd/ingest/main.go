// Command ingest is the FlowScope flow ingestion service.
//
// It listens for NetFlow v5/v9, IPFIX, and sFlow v5 datagrams on UDP,
// decodes each into canonical record.Flow values, and emits them
// through the single record.Emitter fan-out point — to the in-process
// hot ring and to the ClickHouse batch writer.
//
// This early build wires NetFlow v5 + ClickHouse persistence; v9,
// IPFIX, and sFlow land when their parsers are implemented. See
// VISION.md and CLAUDE.md for the full architecture.
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
	chDSN := envOr("FLOWSCOPE_CLICKHOUSE_DSN", "")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Spine: one ring + one emitter for the whole process. The
	// ClickHouse batcher registers as a Sink on the emitter.
	ring := record.NewRing(ringCapacity)
	sinks := []record.Sink{}

	var batcher *store.FlowBatcher
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

		batcher = store.NewFlowBatcher(conn, store.FlowBatcherOptions{
			MaxSize:     5000,
			MaxInterval: 200 * time.Millisecond,
		})
		sinks = append(sinks, batcher)
		slog.Info("clickhouse batcher started", "max_size", 5000, "max_interval", "200ms")
	} else {
		slog.Warn("FLOWSCOPE_CLICKHOUSE_DSN not set — running without persistence (ring only)")
	}

	emitter := record.NewEmitter(ring, sinks...)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := runNetFlowListener(ctx, netflowAddr, emitter); err != nil {
			slog.Error("netflow listener exited", "err", err)
		}
	}()

	slog.Info("flowscope ingest started",
		"netflow", netflowAddr,
		"ring_capacity", ringCapacity,
		"clickhouse", chDSN != "",
	)
	<-ctx.Done()
	slog.Info("shutting down")

	wg.Wait()
	if batcher != nil {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := batcher.Close(shutdownCtx); err != nil {
			slog.Warn("batcher close", "err", err)
		}
	}
	return nil
}

// runNetFlowListener serves the NetFlow / IPFIX UDP port. Today only
// v5 is wired; v9 and IPFIX dispatch lands when those parsers ship.
func runNetFlowListener(ctx context.Context, addr string, emitter *record.Emitter) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	buf := make([]byte, 65535) // max UDP datagram
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
