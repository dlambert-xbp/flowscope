package notifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestDispatcher returns a dispatcher with no ClickHouse connection
// and tight retry timings suitable for unit tests. Use for tests that
// exercise post / deliverWithRetry / SendTest in isolation.
func newTestDispatcher() *Dispatcher {
	return &Dispatcher{
		http:           http.DefaultClient,
		tick:           defaultTick,
		limit:          defaultBatchLimit,
		rescue:         defaultRescueWindow,
		maxAttempts:    3,
		postTimeout:    2 * time.Second,
		initialBackoff: 1 * time.Millisecond,
		nowFn:          func() time.Time { return time.Now().UTC() },
	}
}

func TestDispatcher_DeliverWithRetry_Success(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "http", URL: srv.URL}
	d.deliverWithRetry(context.Background(), ep, sampleEvent())
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 (no retry on 2xx)", got)
	}
}

func TestDispatcher_DeliverWithRetry_RetryThenSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "http", URL: srv.URL}
	d.deliverWithRetry(context.Background(), ep, sampleEvent())
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hits = %d, want 3 (two retries then success)", got)
	}
}

func TestDispatcher_DeliverWithRetry_ExhaustsOn5xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "http", URL: srv.URL}
	d.deliverWithRetry(context.Background(), ep, sampleEvent())
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hits = %d, want 3 (maxAttempts)", got)
	}
}

func TestDispatcher_DeliverWithRetry_NoRetryOn4xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "http", URL: srv.URL}
	d.deliverWithRetry(context.Background(), ep, sampleEvent())
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 (4xx is permanent)", got)
	}
}

func TestDispatcher_DeliverWithRetry_Retry429(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "http", URL: srv.URL}
	d.deliverWithRetry(context.Background(), ep, sampleEvent())
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d, want 2 (429 retries once then succeeds)", got)
	}
}

func TestDispatcher_SendTest_SuccessRecordsCounter(t *testing.T) {
	var (
		hits int32
		body []byte
		ct   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		ct = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
		_ = body
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "slack", URL: srv.URL}
	res, err := d.SendTest(context.Background(), ep)
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if !res.OK {
		t.Errorf("res.OK = false; want true")
	}
	if res.HTTPStatus != http.StatusNoContent {
		t.Errorf("HTTPStatus = %d, want %d", res.HTTPStatus, http.StatusNoContent)
	}
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestDispatcher_SendTest_SkippedByFilter(t *testing.T) {
	d := newTestDispatcher()
	ep := Endpoint{
		ID:             "00000000-0000-0000-0000-000000000001",
		Kind:           "slack",
		URL:            "http://unused",
		SeverityFilter: []string{"critical"},
	}
	res, err := d.SendTest(context.Background(), ep)
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if !res.Skipped {
		t.Errorf("res.Skipped = false; want true (filter excludes info)")
	}
}

func TestDispatcher_SendTest_PropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "http", URL: srv.URL}
	res, err := d.SendTest(context.Background(), ep)
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	if res.OK {
		t.Errorf("res.OK true on 401")
	}
	if res.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("HTTPStatus = %d, want 401", res.HTTPStatus)
	}
}

func TestDispatcher_PostHonorsCustomHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{
		ID:      "00000000-0000-0000-0000-000000000001",
		Kind:    "http",
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer s3cret"},
	}
	d.deliverWithRetry(context.Background(), ep, sampleEvent())
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer s3cret")
	}
}

func TestDispatcher_PostCannotOverrideContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()

	ep := Endpoint{
		ID:      "00000000-0000-0000-0000-000000000001",
		Kind:    "http",
		URL:     srv.URL,
		Headers: map[string]string{"Content-Type": "text/plain"},
	}
	d.deliverWithRetry(context.Background(), ep, sampleEvent())
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (must not be overridable)", gotCT)
	}
}

func TestDispatcher_DeliverWithRetry_RespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := newTestDispatcher()
	d.http = srv.Client()
	d.initialBackoff = 50 * time.Millisecond
	d.maxAttempts = 5

	ep := Endpoint{ID: "00000000-0000-0000-0000-000000000001", Kind: "http", URL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	d.deliverWithRetry(ctx, ep, sampleEvent())
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("deliverWithRetry blocked %v after ctx cancel, want fast return", elapsed)
	}
}
