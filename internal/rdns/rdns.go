// Package rdns provides a small reverse-DNS resolver with an
// in-memory LRU cache and a hard per-lookup timeout. It powers the
// /api/dns/lookup endpoint that decorates flow records on the
// Flows tab and the drill-in drawer.
//
// Design notes:
//
//   - Private IP ranges (RFC 1918, RFC 4193, link-local, loopback,
//     multicast) skip the lookup entirely and return the IP as-is
//     with skipped=true. Reverse PTR records for these blocks are
//     not authoritative anywhere outside a customer's own DNS.
//   - Public IP lookups are cached for HitTTL on success, MissTTL
//     on negative result, ErrTTL on resolver error. Per-IP
//     in-flight singleflight prevents stampedes when the same IP
//     appears in many records.
//   - net.DefaultResolver is used; operators can override the
//     resolver via a custom Resolver value passed to New.
package rdns

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Result is the outcome of a single lookup. Hostname is empty when
// no PTR record exists; Err is non-empty when the resolver returned
// an error (timeout, NXDOMAIN, refused, etc.). Skipped is true for
// addresses we deliberately don't query (private space).
type Result struct {
	IP       string    `json:"ip"`
	Hostname string    `json:"hostname"`
	Err      string    `json:"err,omitempty"`
	Skipped  bool      `json:"skipped"`
	At       time.Time `json:"at"`
}

// Resolver is a narrow interface satisfied by net.DefaultResolver
// (and easy to fake in tests).
type Resolver interface {
	LookupAddr(ctx context.Context, addr string) (names []string, err error)
}

// Options control the cache and per-lookup timeout. Zero values
// pick sensible defaults.
type Options struct {
	HitTTL    time.Duration // cache TTL on successful resolution (default 1h)
	MissTTL   time.Duration // cache TTL on no-PTR (default 5m)
	ErrTTL    time.Duration // cache TTL on resolver error (default 30s)
	Timeout   time.Duration // per-lookup timeout (default 200ms)
	MaxSize   int           // soft cap on cache entries (default 10000); evicted LRU on overflow
	Resolver  Resolver      // override; defaults to net.DefaultResolver
}

func (o Options) defaults() Options {
	if o.HitTTL == 0 {
		o.HitTTL = time.Hour
	}
	if o.MissTTL == 0 {
		o.MissTTL = 5 * time.Minute
	}
	if o.ErrTTL == 0 {
		o.ErrTTL = 30 * time.Second
	}
	if o.Timeout == 0 {
		o.Timeout = 200 * time.Millisecond
	}
	if o.MaxSize == 0 {
		o.MaxSize = 10000
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	return o
}

type entry struct {
	result    Result
	expiresAt time.Time
}

// Resolver is the in-memory cache + per-IP singleflight wrapper.
// Safe for concurrent use.
type Cache struct {
	opts   Options
	mu     sync.Mutex
	by     map[string]*entry
	inflight map[string]*sync.WaitGroup
}

// New builds a cache with the given options.
func New(opts Options) *Cache {
	return &Cache{
		opts:     opts.defaults(),
		by:       make(map[string]*entry),
		inflight: make(map[string]*sync.WaitGroup),
	}
}

// Lookup resolves ip and caches the result. Repeated calls for the
// same IP within the cache TTL return immediately. Caller-supplied
// ctx bounds total wait time including any in-flight lookup the
// caller is sharing with another goroutine.
func (c *Cache) Lookup(ctx context.Context, ip string) Result {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return Result{IP: ip, Err: "invalid IP", At: time.Now().UTC()}
	}
	if isPrivate(addr) {
		return Result{IP: ip, Skipped: true, At: time.Now().UTC()}
	}
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.by[ip]; ok && now.Before(e.expiresAt) {
		c.mu.Unlock()
		return e.result
	}
	if wg, busy := c.inflight[ip]; busy {
		c.mu.Unlock()
		// Wait for the in-flight resolver, then return cached
		// result. Bound by ctx — caller can give up if the lookup
		// is taking too long.
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			return Result{IP: ip, Err: ctx.Err().Error(), At: time.Now().UTC()}
		}
		c.mu.Lock()
		if e, ok := c.by[ip]; ok {
			c.mu.Unlock()
			return e.result
		}
		c.mu.Unlock()
		// Fall through and resolve again — rare race where the
		// cached entry was evicted between the wg signal and our
		// re-acquire.
	} else {
		c.mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(1)
	c.mu.Lock()
	c.inflight[ip] = &wg
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, ip)
		c.mu.Unlock()
		wg.Done()
	}()

	lookupCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	names, lookupErr := c.opts.Resolver.LookupAddr(lookupCtx, ip)
	r := Result{IP: ip, At: time.Now().UTC()}
	var ttl time.Duration
	if lookupErr != nil {
		r.Err = lookupErr.Error()
		ttl = c.opts.ErrTTL
	} else if len(names) == 0 {
		ttl = c.opts.MissTTL
	} else {
		// Trim the trailing dot the stdlib leaves on FQDN PTR
		// answers — it's noise in the UI.
		first := names[0]
		if n := len(first); n > 0 && first[n-1] == '.' {
			first = first[:n-1]
		}
		r.Hostname = first
		ttl = c.opts.HitTTL
	}
	c.mu.Lock()
	c.by[ip] = &entry{result: r, expiresAt: time.Now().Add(ttl)}
	c.evictIfFullLocked()
	c.mu.Unlock()
	return r
}

// LookupBatch resolves multiple IPs concurrently and returns a
// map keyed by the input IP string. Skipped + cached entries
// resolve instantly; new lookups all happen in parallel bounded
// by the same ctx.
func (c *Cache) LookupBatch(ctx context.Context, ips []string) map[string]Result {
	out := make(map[string]Result, len(ips))
	if len(ips) == 0 {
		return out
	}
	type pair struct {
		ip string
		r  Result
	}
	ch := make(chan pair, len(ips))
	var wg sync.WaitGroup
	for _, ip := range ips {
		if _, ok := out[ip]; ok {
			continue // de-dup the input
		}
		out[ip] = Result{} // placeholder so we don't double-fire
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			ch <- pair{addr, c.Lookup(ctx, addr)}
		}(ip)
	}
	go func() { wg.Wait(); close(ch) }()
	for p := range ch {
		out[p.ip] = p.r
	}
	return out
}

// evictIfFullLocked drops the oldest expiry-time entries when the
// cache exceeds MaxSize. Cheap O(N) sort; acceptable at this size.
// Caller must hold c.mu.
func (c *Cache) evictIfFullLocked() {
	if len(c.by) <= c.opts.MaxSize {
		return
	}
	// Simple LRU-ish: drop everything past the soft cap by
	// expiry time. Not strictly LRU but matches the user's
	// expectation — entries that have been around longest go
	// first.
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(c.by))
	for k, e := range c.by {
		all = append(all, kv{k, e.expiresAt})
	}
	// Partial sort would be faster; full sort is fine at <10K.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j-1].t.After(all[j].t); j-- {
			all[j-1], all[j] = all[j], all[j-1]
		}
	}
	excess := len(all) - c.opts.MaxSize
	for i := 0; i < excess; i++ {
		delete(c.by, all[i].k)
	}
}

// isPrivate returns true for IPs we deliberately don't resolve:
// RFC 1918, RFC 4193 (fc00::/7), link-local, loopback, multicast,
// and the unspecified address. Reverse PTR records for these
// blocks are not authoritative outside a customer's own DNS, so
// querying public DNS for them is at best wasted, at worst leaks
// internal addressing.
func isPrivate(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	if a.IsPrivate() {
		return true
	}
	return false
}
