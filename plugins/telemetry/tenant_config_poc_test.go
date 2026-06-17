package telemetry

import (
	"math"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tenant-scoped telemetry config POC — see doc 09 once written.
//
// Today the telemetry plugin emits 17 metric vectors with a `tenant_id`
// label, all process-global: same metrics, same labels, same sample
// rate (100%) for every tenant. Tenant-axis configuration of the
// emission behavior is what this POC fills in.
//
// Scope (validated in this POC):
//   1. Per-tenant SAMPLE RATE — dial back emission for a noisy tenant.
//   2. Per-tenant METRIC DENY LIST — skip specific metric vectors for a
//      privacy-conscious tenant.
//   3. Per-tenant LABEL DENY LIST — strip specific labels from emitted
//      label sets (Prom convention: replace with empty string so the
//      label slot is preserved but the value is hidden).
//
// What this POC deliberately doesn't model:
//   - configstore persistence. The TenantConfigStore is an in-memory
//     map; production swaps in a DB-backed implementation reading from
//     a `telemetry_configs` table keyed by tenant_id. Doc 09 covers
//     the production wiring proposal.
//   - Per-tenant push gateway destinations. Bigger surface (need to
//     fan out to N pushers); deferred.
//   - The actual wiring into PrometheusPlugin's emission points. The
//     POC validates the POLICY shape; patch-equivalent wiring would
//     guard every metric.With(Label)Values call with policy.Decide.
//     The doc proposes a small Emit() wrapper that centralizes the
//     decision.

// TenantTelemetryConfig is the per-tenant config the policy reads. nil
// means "no config" → default (today's) behavior. Defined as a struct so
// future fields (cardinality cap, retention, exporter override) extend
// the shape without breaking the API.
type TenantTelemetryConfig struct {
	// SampleRate ∈ [0,1]. nil pointer = use default (always emit).
	// Non-nil 1.0 = explicit always-emit. Non-nil 0.0 = explicit drop-all
	// kill switch. Sub-1.0 non-nil values are decided per-emission
	// against the injected rand source.
	//
	// Pointer type because Go's zero value for float64 is 0.0 and we
	// need to distinguish "explicit 0 (drop all)" from "field not set
	// in this config (use default)". Without this, a config that only
	// sets MetricDenyList would silently drop every metric.
	SampleRate *float64

	// MetricDenyList — metric names to skip entirely for this tenant.
	// Match against the plugin's exported metric name (e.g.
	// "upstream_requests_total"). Case-sensitive exact match.
	MetricDenyList []string

	// LabelDenyList — label names whose value gets replaced with "" before
	// emission. The label slot stays (Prometheus requires every metric in
	// a vec to have the same label cardinality), but the value is hidden.
	LabelDenyList []string
}

// floatPtr is a test helper. Production code would build configs from
// configstore rows where the pointer-vs-nil distinction comes naturally
// from a NULL column.
func floatPtr(f float64) *float64 { return &f }

// TenantConfigStore abstracts where the per-tenant config comes from.
// Production: configstore-backed cache, refreshed on tenant evict.
// Tests: inMemoryTenantConfigStore below.
type TenantConfigStore interface {
	Get(tenantID string) *TenantTelemetryConfig
}

// inMemoryTenantConfigStore is the POC implementation — a static map.
type inMemoryTenantConfigStore struct {
	mu       sync.RWMutex
	configs  map[string]*TenantTelemetryConfig
}

func newInMemoryTenantConfigStore() *inMemoryTenantConfigStore {
	return &inMemoryTenantConfigStore{configs: map[string]*TenantTelemetryConfig{}}
}

func (s *inMemoryTenantConfigStore) Get(tenantID string) *TenantTelemetryConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configs[tenantID]
}

func (s *inMemoryTenantConfigStore) Set(tenantID string, cfg *TenantTelemetryConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[tenantID] = cfg
}

// TelemetryPolicy is the choke point every emission flows through. One
// Decide() call per (metric, tenant, labels) tuple. Returns whether to
// emit, and (when emit==true) the label set to use — which may differ
// from the input if the tenant has a label deny list.
type TelemetryPolicy struct {
	store TenantConfigStore
	// nextRand is injectable so tests can drive deterministic outcomes.
	// Production wires in rand.Float64.
	nextRand func() float64
}

func NewTelemetryPolicy(store TenantConfigStore, nextRand func() float64) *TelemetryPolicy {
	return &TelemetryPolicy{store: store, nextRand: nextRand}
}

// Decide returns (emit, labelsToUse). When emit is false, the caller
// MUST NOT call the underlying Prometheus metric — Decide already
// applied the policy. When emit is true, labelsToUse is the label set
// to pass to Prometheus (may be the input unchanged if no policy applies).
//
// Hot-path: this is called per metric emission. The lookups are O(N)
// over deny-list lengths, which v1 expects to be small (< 10 entries).
// If we ever observe > 100 entries the obvious next step is to
// pre-hash the lists at config-load time.
func (p *TelemetryPolicy) Decide(metricName, tenantID string, labels map[string]string) (emit bool, labelsToUse map[string]string) {
	cfg := p.store.Get(tenantID)
	if cfg == nil {
		return true, labels
	}
	if contains(cfg.MetricDenyList, metricName) {
		return false, nil
	}
	if cfg.SampleRate != nil {
		rate := *cfg.SampleRate
		switch {
		case rate <= 0:
			return false, nil
		case rate < 1.0 && p.nextRand() >= rate:
			return false, nil
		}
	}
	if len(cfg.LabelDenyList) > 0 {
		out := make(map[string]string, len(labels))
		for k, v := range labels {
			if contains(cfg.LabelDenyList, k) {
				out[k] = ""
			} else {
				out[k] = v
			}
		}
		return true, out
	}
	return true, labels
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ============================================================================
// Tests — each scenario validates one policy axis end-to-end.
// ============================================================================

// TestPolicy_NoConfigEmitsEverything is the regression guard: a tenant
// with no config row gets today's behavior (always emit, full labels).
func TestPolicy_NoConfigEmitsEverything(t *testing.T) {
	store := newInMemoryTenantConfigStore()
	policy := NewTelemetryPolicy(store, func() float64 { return 0.5 })

	labels := map[string]string{"tenant_id": "no-config", "provider": "openai", "model": "gpt-4o"}
	emit, out := policy.Decide("upstream_requests_total", "no-config", labels)
	require.True(t, emit)
	require.Equal(t, labels, out, "tenant with no config gets unchanged labels")
}

// TestPolicy_SampleRateZeroDropsAll — explicit kill switch. A tenant with
// SampleRate=0 has no metrics emitted, period. Cheaper than a full deny
// list when an operator just wants to silence a noisy test tenant.
func TestPolicy_SampleRateZeroDropsAll(t *testing.T) {
	store := newInMemoryTenantConfigStore()
	store.Set("noisy-test", &TenantTelemetryConfig{SampleRate: floatPtr(0.0)})
	policy := NewTelemetryPolicy(store, func() float64 { return 0.0 }) // wouldn't matter

	emit, _ := policy.Decide("upstream_requests_total", "noisy-test", map[string]string{"tenant_id": "noisy-test"})
	require.False(t, emit)
}

// TestPolicy_SampleRatePartialKeepsFraction — over many trials, the
// emitted fraction should be within tolerance of the configured rate.
func TestPolicy_SampleRatePartialKeepsFraction(t *testing.T) {
	store := newInMemoryTenantConfigStore()
	store.Set("partial", &TenantTelemetryConfig{SampleRate: floatPtr(0.20)})

	// Deterministic stream of "uniform" draws using a linear progression
	// over [0,1). Tests the boundary condition (r < SampleRate) without
	// relying on math/rand seed semantics.
	var calls int
	policy := NewTelemetryPolicy(store, func() float64 {
		f := float64(calls%100) / 100.0
		calls++
		return f
	})

	const trials = 1000
	emitted := 0
	for i := 0; i < trials; i++ {
		emit, _ := policy.Decide("upstream_requests_total", "partial", map[string]string{"tenant_id": "partial"})
		if emit {
			emitted++
		}
	}
	// With the linear draw and SampleRate=0.20, exactly 20% should pass:
	// 20 of every 100 draws (0..0.19) are < 0.20.
	fraction := float64(emitted) / float64(trials)
	require.InDelta(t, 0.20, fraction, 0.001, "20% sample rate should emit ~20% of requests (got %.3f)", fraction)
}

// TestPolicy_MetricDenyListSkipsNamed — a privacy-conscious tenant turns
// off specific metrics (e.g. exclude prompt-body size histograms).
func TestPolicy_MetricDenyListSkipsNamed(t *testing.T) {
	store := newInMemoryTenantConfigStore()
	store.Set("privacy-strict", &TenantTelemetryConfig{
		MetricDenyList: []string{"http_request_size_bytes", "http_response_size_bytes"},
	})
	policy := NewTelemetryPolicy(store, func() float64 { return 0.0 })

	labels := map[string]string{"tenant_id": "privacy-strict"}

	// Denied metrics: dropped.
	for _, name := range []string{"http_request_size_bytes", "http_response_size_bytes"} {
		emit, _ := policy.Decide(name, "privacy-strict", labels)
		require.False(t, emit, "metric %s should be denied", name)
	}
	// Other metrics: still emitted, labels intact.
	emit, out := policy.Decide("upstream_requests_total", "privacy-strict", labels)
	require.True(t, emit)
	require.Equal(t, labels, out)
}

// TestPolicy_LabelDenyListStripsValues — strip the label value (replace
// with "") but keep the label slot, per Prometheus convention.
//
// Why empty-string instead of removing the slot: Prometheus vec metrics
// require all observations on a vec to carry the SAME set of label
// names. Removing a slot for one tenant means Prom rejects the
// observation (different cardinality). Empty value preserves the slot.
func TestPolicy_LabelDenyListStripsValues(t *testing.T) {
	store := newInMemoryTenantConfigStore()
	store.Set("hide-customer", &TenantTelemetryConfig{
		LabelDenyList: []string{"customer_name", "customer_id"},
	})
	policy := NewTelemetryPolicy(store, func() float64 { return 0.0 })

	labels := map[string]string{
		"tenant_id":     "hide-customer",
		"provider":      "openai",
		"customer_id":   "cust-123",
		"customer_name": "Acme Corp",
	}
	emit, out := policy.Decide("upstream_requests_total", "hide-customer", labels)
	require.True(t, emit)
	require.Equal(t, "openai", out["provider"], "non-denied labels preserved")
	require.Equal(t, "hide-customer", out["tenant_id"], "non-denied labels preserved")
	require.Equal(t, "", out["customer_id"], "denied label value stripped")
	require.Equal(t, "", out["customer_name"], "denied label value stripped")

	// Slot preservation: the same label NAMES must still be present
	// (Prometheus cardinality requirement).
	wantKeys := []string{"customer_id", "customer_name", "provider", "tenant_id"}
	gotKeys := make([]string, 0, len(out))
	for k := range out {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	require.Equal(t, wantKeys, gotKeys, "label keys must match input — only values are hidden")
}

// TestPolicy_CombinedKnobsCompose — exercises a realistic config:
// 50% sampling + deny one metric + hide one label. All three apply in
// the expected order (deny short-circuits before sampling; sampling
// short-circuits before label rewriting).
func TestPolicy_CombinedKnobsCompose(t *testing.T) {
	store := newInMemoryTenantConfigStore()
	store.Set("mixed", &TenantTelemetryConfig{
		SampleRate:     floatPtr(0.50),
		MetricDenyList: []string{"http_request_size_bytes"},
		LabelDenyList:  []string{"customer_name"},
	})

	// Deterministic random: 0.25 always (< 0.5, so sampling lets it through).
	policy := NewTelemetryPolicy(store, func() float64 { return 0.25 })

	labels := map[string]string{"tenant_id": "mixed", "customer_name": "Acme Corp", "provider": "openai"}

	// Denied metric: short-circuit BEFORE sampling and label processing.
	emit, _ := policy.Decide("http_request_size_bytes", "mixed", labels)
	require.False(t, emit, "denied metric short-circuits")

	// Allowed metric: passes sampling (0.25 < 0.5), then label rewrite.
	emit, out := policy.Decide("upstream_requests_total", "mixed", labels)
	require.True(t, emit)
	require.Equal(t, "", out["customer_name"], "label deny still applies after sampling pass")
	require.Equal(t, "openai", out["provider"])

	// Now bump rand above the rate so sampling drops it. Label rewrite
	// shouldn't even run.
	policy = NewTelemetryPolicy(store, func() float64 { return 0.75 })
	emit, out = policy.Decide("upstream_requests_total", "mixed", labels)
	require.False(t, emit, "sample drop short-circuits — labels not rewritten")
	require.Nil(t, out)
}

// TestPolicy_ConcurrentSafe is the data-race canary. The store + policy
// must tolerate concurrent Decide and Set under the race detector.
func TestPolicy_ConcurrentSafe(t *testing.T) {
	store := newInMemoryTenantConfigStore()
	store.Set("tenant-a", &TenantTelemetryConfig{SampleRate: floatPtr(1.0)})
	policy := NewTelemetryPolicy(store, func() float64 { return 0.0 })

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			policy.Decide("upstream_requests_total", "tenant-a", map[string]string{"tenant_id": "tenant-a"})
		}()
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(rate float64) {
			defer wg.Done()
			store.Set("tenant-a", &TenantTelemetryConfig{SampleRate: floatPtr(rate)})
		}(math.Mod(float64(i)/10.0, 1.0))
	}
	wg.Wait()
}
