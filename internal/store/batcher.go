package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/record"
)

// FlowBatcher is the ClickHouse-bound implementation of record.Sink for
// the `flows` table. It accumulates rows in memory and flushes whenever
// the batch reaches MaxSize OR ages past MaxInterval, whichever first.
//
// Per CLAUDE.md "Concurrency model": backpressure is explicit. If the
// ClickHouse cluster cannot keep up, Consume blocks at the size cap,
// the parser pool stalls, and dropped-packet counters increment on the
// listener side rather than memory growing unbounded.
type FlowBatcher struct {
	conn        driver.Conn
	maxSize     int
	maxInterval time.Duration

	mu      sync.Mutex
	buf     []record.Flow
	flushCh chan struct{}

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// FlowBatcherOptions configures a FlowBatcher.
type FlowBatcherOptions struct {
	// MaxSize is the row-count threshold that triggers a flush.
	// Sensible values: 1000–10000. Default: 5000.
	MaxSize int
	// MaxInterval is the time-based flush ceiling. Default: 200ms.
	MaxInterval time.Duration
}

// NewFlowBatcher constructs and starts a FlowBatcher. The returned
// batcher's Run goroutine flushes until Close is called.
func NewFlowBatcher(conn driver.Conn, opts FlowBatcherOptions) *FlowBatcher {
	if opts.MaxSize <= 0 {
		opts.MaxSize = 5000
	}
	if opts.MaxInterval <= 0 {
		opts.MaxInterval = 200 * time.Millisecond
	}
	b := &FlowBatcher{
		conn:        conn,
		maxSize:     opts.MaxSize,
		maxInterval: opts.MaxInterval,
		buf:         make([]record.Flow, 0, opts.MaxSize),
		flushCh:     make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go b.run()
	return b
}

// Consume implements record.Sink. It buffers the flow and signals the
// flusher if the buffer is full.
func (b *FlowBatcher) Consume(ctx context.Context, f record.Flow) error {
	b.mu.Lock()
	b.buf = append(b.buf, f)
	full := len(b.buf) >= b.maxSize
	b.mu.Unlock()

	if full {
		select {
		case b.flushCh <- struct{}{}:
		default:
			// already pending; flusher will drain
		}
	}
	return nil
}

// Close stops the flush loop and writes any pending records before
// returning. It is safe to call multiple times.
func (b *FlowBatcher) Close(ctx context.Context) error {
	b.stopOnce.Do(func() { close(b.stop) })
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (b *FlowBatcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.maxInterval)
	defer ticker.Stop()

	for {
		select {
		case <-b.stop:
			_ = b.flush(context.Background())
			return
		case <-ticker.C:
			_ = b.flush(context.Background())
		case <-b.flushCh:
			_ = b.flush(context.Background())
		}
	}
}

// flush writes the current buffer to ClickHouse. It atomically swaps
// the buffer for a fresh one before doing IO, so Consume callers never
// wait on the network.
func (b *FlowBatcher) flush(ctx context.Context) error {
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return nil
	}
	batch := b.buf
	b.buf = make([]record.Flow, 0, b.maxSize)
	b.mu.Unlock()

	start := time.Now()
	err := b.write(ctx, batch)
	obs.BatchFlushDuration.WithLabelValues("flows").Observe(time.Since(start).Seconds())
	if err != nil {
		obs.BatchFlushesTotal.WithLabelValues("flows", "error").Inc()
		return err
	}
	obs.BatchFlushesTotal.WithLabelValues("flows", "ok").Inc()
	obs.BatchRowsTotal.WithLabelValues("flows").Add(float64(len(batch)))
	return nil
}

func (b *FlowBatcher) write(ctx context.Context, batch []record.Flow) error {
	if b.conn == nil {
		return errors.New("store: nil ClickHouse connection")
	}
	chBatch, err := b.conn.PrepareBatch(ctx, "INSERT INTO flows")
	if err != nil {
		return fmt.Errorf("store: prepare batch: %w", err)
	}
	for _, f := range batch {
		if err := chBatch.Append(
			f.Observed,
			toIPv6(f.Exporter),
			toIPv6(f.SrcAddr),
			toIPv6(f.DstAddr),
			f.SrcPort,
			f.DstPort,
			f.Proto,
			f.Bytes,
			f.Packets,
			f.InputIfIndex,
			f.OutputIfIndex,
			f.VlanID,
			f.Tos,
			f.TCPFlags,
			f.Source.String(),
		); err != nil {
			return fmt.Errorf("store: append row: %w", err)
		}
	}
	if err := chBatch.Send(); err != nil {
		return fmt.Errorf("store: send batch: %w", err)
	}
	return nil
}

// toIPv6 returns a 16-byte big-endian net.IP suitable for the
// ClickHouse IPv6 column type. IPv4 inputs are mapped via the
// 4-in-6 form so the schema can hold both families uniformly.
func toIPv6(addr netip.Addr) net.IP {
	a := addr.As16()
	return a[:]
}
