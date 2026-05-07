package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/record"
)

// CounterBatcher is the ClickHouse-bound implementation of
// record.CounterSink for the iface_counter_samples table. It mirrors
// FlowBatcher: flushes whenever the batch reaches MaxSize OR ages past
// MaxInterval.
//
// Counter samples arrive at much lower rates than flow samples
// (typically 1 row per interface per second), so default sizing is
// correspondingly lower than the flow batcher.
type CounterBatcher struct {
	conn        driver.Conn
	maxSize     int
	maxInterval time.Duration

	mu      sync.Mutex
	buf     []record.CounterSample
	flushCh chan struct{}

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// CounterBatcherOptions configures a CounterBatcher.
type CounterBatcherOptions struct {
	// MaxSize is the row-count flush threshold. Default: 500.
	MaxSize int
	// MaxInterval is the time-based flush ceiling. Default: 1s.
	MaxInterval time.Duration
}

// NewCounterBatcher constructs and starts a CounterBatcher. The
// returned batcher's flush goroutine runs until Close.
func NewCounterBatcher(conn driver.Conn, opts CounterBatcherOptions) *CounterBatcher {
	if opts.MaxSize <= 0 {
		opts.MaxSize = 500
	}
	if opts.MaxInterval <= 0 {
		opts.MaxInterval = time.Second
	}
	b := &CounterBatcher{
		conn:        conn,
		maxSize:     opts.MaxSize,
		maxInterval: opts.MaxInterval,
		buf:         make([]record.CounterSample, 0, opts.MaxSize),
		flushCh:     make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go b.run()
	return b
}

// Consume implements record.CounterSink. Buffers the sample and
// signals the flusher if the buffer is at capacity.
func (b *CounterBatcher) Consume(ctx context.Context, c record.CounterSample) error {
	b.mu.Lock()
	b.buf = append(b.buf, c)
	full := len(b.buf) >= b.maxSize
	b.mu.Unlock()
	if full {
		select {
		case b.flushCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// Close stops the flush loop and writes any pending samples before
// returning. Safe to call multiple times.
func (b *CounterBatcher) Close(ctx context.Context) error {
	b.stopOnce.Do(func() { close(b.stop) })
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (b *CounterBatcher) run() {
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

func (b *CounterBatcher) flush(ctx context.Context) error {
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return nil
	}
	batch := b.buf
	b.buf = make([]record.CounterSample, 0, b.maxSize)
	b.mu.Unlock()

	start := time.Now()
	err := b.write(ctx, batch)
	obs.BatchFlushDuration.WithLabelValues("iface_counter_samples").Observe(time.Since(start).Seconds())
	if err != nil {
		obs.BatchFlushesTotal.WithLabelValues("iface_counter_samples", "error").Inc()
		return err
	}
	obs.BatchFlushesTotal.WithLabelValues("iface_counter_samples", "ok").Inc()
	obs.BatchRowsTotal.WithLabelValues("iface_counter_samples").Add(float64(len(batch)))
	return nil
}

func (b *CounterBatcher) write(ctx context.Context, batch []record.CounterSample) error {
	if b.conn == nil {
		return errors.New("store: nil ClickHouse connection")
	}
	chBatch, err := b.conn.PrepareBatch(ctx, "INSERT INTO iface_counter_samples")
	if err != nil {
		return fmt.Errorf("store: prepare counter batch: %w", err)
	}
	for _, c := range batch {
		if err := chBatch.Append(
			c.Observed,
			toIPv6(c.Exporter),
			c.IfIndex,
			c.InOctets,
			c.OutOctets,
			c.InPackets,
			c.OutPackets,
			c.InErrors,
			c.OutErrors,
			c.InDiscards,
			c.OutDiscards,
		); err != nil {
			return fmt.Errorf("store: append counter row: %w", err)
		}
	}
	if err := chBatch.Send(); err != nil {
		return fmt.Errorf("store: send counter batch: %w", err)
	}
	return nil
}
