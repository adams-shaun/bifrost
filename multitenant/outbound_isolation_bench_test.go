package multitenant

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Outbound HTTP topology benchmark — backs the analysis in
// docs/multi-tenant-f5/08-outbound-isolation.md.
//
// The patch7 multi-tenant model gives each tenant runtime its own
// *bifrost.Bifrost, which constructs its own provider instances, each with
// its own *fasthttp.Client and connection pool. So outbound HTTP IS
// per-tenant today by accident — *at the cost* of N pools when N tenants
// share an upstream host.
//
// This benchmark quantifies the trade-off across three topologies:
//
//   (A) PER-TENANT clients (patch7 today)
//       Each tenant has its own *http.Client + *http.Transport. No shared
//       state in the conn pool. Fully isolated. Resource cost scales with
//       N tenants × MaxConnsPerHost.
//
//   (B) SHARED client (counterfactual consolidation)
//       Both tenants funnel through one *http.Client. One conn pool keyed
//       by (scheme, host). Resource-efficient (one pool) BUT slow tenants
//       hold conn slots and starve fast tenants. Classic noisy neighbor.
//
//   (C) SHARED client + per-tenant in-flight cap (hybrid)
//       One *http.Client shared, but each tenant's requests pass through
//       a tenant-scoped semaphore that caps simultaneous in-flight calls.
//       Tenant A can't hog every slot; tenant B is bounded but not
//       starved. Best of both worlds at modest complexity cost.
//
// The upstream is ONE host (realistic SaaS case: both tenants use the
// same OpenAI-compatible vLLM cluster). MaxConnsPerHost is tight (4) so
// contention is visible at small concurrency. Tenant A's requests sleep
// 200ms server-side (simulating an overloaded upstream); tenant B's
// return immediately.
//
// Numbers below are wall-clock from `go test -run TestOutboundTopology -v`
// on the workstation that produced them — the SHAPE of the difference is
// what matters, not the absolute numbers.

const (
	benchRequestsPerTenant = 50
	benchMaxConnsPerHost   = 4
	benchSlowSleep         = 200 * time.Millisecond
)

type tenantResult struct {
	name      string
	latencies []time.Duration
}

func (r *tenantResult) percentile(p float64) time.Duration {
	if len(r.latencies) == 0 {
		return 0
	}
	idx := int(float64(len(r.latencies)-1) * p)
	return r.latencies[idx]
}

func (r *tenantResult) sort() {
	sort.Slice(r.latencies, func(i, j int) bool { return r.latencies[i] < r.latencies[j] })
}

// upstreamHandler simulates a vLLM that returns immediately for tenant B
// but sleeps for tenant A (the "tenant A's upstream is overloaded" case).
func upstreamHandler(activeConns *int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(activeConns, 1)
		defer atomic.AddInt64(activeConns, -1)
		if r.Header.Get("X-Bench-Tenant") == "A" {
			time.Sleep(benchSlowSleep)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

func newTransport() *http.Transport {
	// Settings line up roughly with what bifrost's fasthttp.Client carries.
	return &http.Transport{
		MaxConnsPerHost:     benchMaxConnsPerHost,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   false,
	}
}

// fireRequestsConcurrently runs N parallel requests for one tenant.
// Each request fires its own goroutine — this is what saturates the
// connection pool. Without concurrency the contention story doesn't show.
func fireRequestsConcurrently(t *testing.T, url string, client *http.Client, tenant string, wg *sync.WaitGroup, out *tenantResult, outMu *sync.Mutex) {
	defer wg.Done()
	var inner sync.WaitGroup
	for i := 0; i < benchRequestsPerTenant; i++ {
		inner.Add(1)
		go func() {
			defer inner.Done()
			start := time.Now()
			req, _ := http.NewRequest("POST", url, nil)
			req.Header.Set("X-Bench-Tenant", tenant)
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			dur := time.Since(start)
			outMu.Lock()
			out.latencies = append(out.latencies, dur)
			outMu.Unlock()
		}()
	}
	inner.Wait()
}

// runScenario fires `benchRequestsPerTenant` concurrent requests from each
// tenant against a single upstream and returns the latency distribution
// per tenant.
//
// startGate is a barrier-style channel: a single buffered channel closed
// when both tenants should burst simultaneously — guarantees overlap.
func runScenario(t *testing.T, upstreamURL string, clientA, clientB *http.Client, semA, semB chan struct{}) (resA, resB *tenantResult) {
	resA = &tenantResult{name: "tenant A (slow upstream)"}
	resB = &tenantResult{name: "tenant B (fast upstream)"}
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Pre-buffer one token per request so the semaphore (when provided)
	// gates concurrency rather than just sequencing them all.
	prepSem := func(sem chan struct{}, cap int) {
		if sem == nil {
			return
		}
		for i := 0; i < cap; i++ {
			sem <- struct{}{}
		}
	}
	prepSem(semA, cap(semA))
	prepSem(semB, cap(semB))

	startGate := make(chan struct{})

	wg.Add(2)
	go func() {
		<-startGate
		fireRequestsConcurrently(t, upstreamURL, clientA, "A", &wg, resA, &mu)
	}()
	go func() {
		<-startGate
		fireRequestsConcurrently(t, upstreamURL, clientB, "B", &wg, resB, &mu)
	}()
	time.Sleep(10 * time.Millisecond)
	close(startGate)

	wg.Wait()
	resA.sort()
	resB.sort()
	return resA, resB
}

func reportScenario(t *testing.T, label string, a, b *tenantResult) {
	t.Logf("--- %s ---", label)
	t.Logf("  %-30s n=%d  p50=%6s  p95=%6s  p99=%6s  max=%6s",
		a.name, len(a.latencies),
		a.percentile(0.50).Round(time.Millisecond),
		a.percentile(0.95).Round(time.Millisecond),
		a.percentile(0.99).Round(time.Millisecond),
		a.percentile(1.00).Round(time.Millisecond),
	)
	t.Logf("  %-30s n=%d  p50=%6s  p95=%6s  p99=%6s  max=%6s",
		b.name, len(b.latencies),
		b.percentile(0.50).Round(time.Millisecond),
		b.percentile(0.95).Round(time.Millisecond),
		b.percentile(0.99).Round(time.Millisecond),
		b.percentile(1.00).Round(time.Millisecond),
	)
}

// TestOutboundTopology_BenchmarkNoisyNeighborTradeoffs is the entry point.
// Runs the three topologies against the same upstream and prints the
// per-tenant latency distribution. Reading the output is the demo.
//
// Run with:
//
//	go test ./multitenant -run TestOutboundTopology -v
func TestOutboundTopology_BenchmarkNoisyNeighborTradeoffs(t *testing.T) {
	var activeConns int64
	upstream := httptest.NewServer(upstreamHandler(&activeConns))
	defer upstream.Close()

	t.Logf("upstream: %s   MaxConnsPerHost=%d   tenant A sleep=%v   requests/tenant=%d",
		upstream.URL, benchMaxConnsPerHost, benchSlowSleep, benchRequestsPerTenant)

	// (A) PER-TENANT clients — patch7 today.
	{
		mkClient := func() *http.Client { return &http.Client{Transport: newTransport(), Timeout: 10 * time.Second} }
		a, b := runScenario(t, upstream.URL, mkClient(), mkClient(), nil, nil)
		reportScenario(t, "(A) PER-TENANT clients (patch7 today)", a, b)
	}

	// (B) SHARED client — the noisy-neighbor counterfactual.
	{
		shared := &http.Client{Transport: newTransport(), Timeout: 10 * time.Second}
		a, b := runScenario(t, upstream.URL, shared, shared, nil, nil)
		reportScenario(t, "(B) SHARED client (consolidation, no quotas)", a, b)
	}

	// (C) SHARED client + per-tenant semaphore — the hybrid.
	{
		shared := &http.Client{Transport: newTransport(), Timeout: 10 * time.Second}
		semA := make(chan struct{}, 2) // per-tenant in-flight cap of 2
		semB := make(chan struct{}, 2)
		// Modify runRequests to put a token back after the call returns so
		// the semaphore actually gates throughput, not just startup.
		// We embed that via a wrapping client.
		wrap := func(c *http.Client, sem chan struct{}) *http.Client {
			return &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if sem != nil {
						<-sem                              // take token (blocks if cap reached)
						defer func() { sem <- struct{}{} }() // return token
					}
					return c.Transport.RoundTrip(req)
				}),
				Timeout: c.Timeout,
			}
		}
		a, b := runScenario(t, upstream.URL,
			wrap(shared, semA), wrap(shared, semB),
			semA, semB)
		reportScenario(t, "(C) SHARED client + per-tenant semaphore (hybrid)", a, b)
	}
}

// roundTripFunc lets us cheaply wrap http.RoundTripper for the (C)
// scenario's per-tenant semaphore gate. Equivalent of fasthttp's
// pre-request hook.
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
