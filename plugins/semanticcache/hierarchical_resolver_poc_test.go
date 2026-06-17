package semanticcache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Hierarchical policy resolution POC — see doc 11.
//
// Doc 10's POC put per-tenant config on a TenantSemanticCacheConfig
// struct. That's the second layer (tenant override). Real deployments
// also want:
//
//   - Default global settings (plugin defaults)
//   - Per-tenant override (covered by doc 10)
//   - Per-VK override (most specific — same VK every time gets the same
//     specialized policy)
//   - Customer / team override (the VK's owning entity — multiple VKs
//     of one customer share the customer's policy)
//
// The resolution order is "most specific wins" with merge semantics on
// the leaves (Threshold/TTL/Enabled). A field set at a more specific
// layer overrides the same field set at any less specific layer; an
// unset field at a more specific layer means "inherit from the next
// less specific layer."
//
// This mirrors governance's BudgetResolver hierarchy (provider → model
// → customer → team → user → VK), which is the established f5xc
// pattern for this kind of resolution.
//
// What the POC proves:
//   1. Plugin defaults reach a request that has no tenant/VK on ctx
//      (OSS path).
//   2. Tenant config overrides plugin defaults at the matching field
//      level only (other fields stay at plugin default).
//   3. Customer/team config overrides tenant config (when VK has a
//      customer/team and that entity has a config row).
//   4. VK-specific config overrides everything else (most specific).
//   5. Partial overrides COMPOSE — VK sets only TTL, customer sets
//      only Threshold, tenant sets only Enabled → the resolved policy
//      carries one field from each layer plus plugin defaults for the
//      rest.
//   6. Concurrency canary under race detector.

// =============================================================================
// Resolution shape — same TenantSemanticCacheConfig leaf type, but
// stored at four layers with four lookup interfaces.
// =============================================================================

// PluginDefaults is the bottom layer (process-global from the plugin's
// own Config). Concrete values, never nil pointers — this is the
// final fallback when nothing else applies.
type PluginDefaults struct {
	Threshold float64
	TTL       time.Duration
	Enabled   bool
}

// VKResolverInput is what the resolver gets per request: the VK id
// (from the BifrostContext), and the metadata it pulls out of the VK
// row that lets us look up customer/team configs.
type VKResolverInput struct {
	VKID       string
	TenantID   string
	CustomerID *string // VK's owning customer, if any
	TeamID     *string // VK's owning team, if any
}

// PerVKConfigStore — top of the hierarchy. The VK row's
// SemanticCacheConfig column, if any.
type PerVKConfigStore interface {
	Get(vkID string) *TenantSemanticCacheConfig
}

// PerCustomerConfigStore — second from top. The customer's
// SemanticCacheConfig column.
type PerCustomerConfigStore interface {
	Get(customerID string) *TenantSemanticCacheConfig
}

// PerTeamConfigStore — third from top.
type PerTeamConfigStore interface {
	Get(teamID string) *TenantSemanticCacheConfig
}

// PerTenantConfigStore — fourth from top (above plugin defaults).
// Same as doc 10's TenantCacheConfigStore.
type PerTenantConfigStore interface {
	Get(tenantID string) *TenantSemanticCacheConfig
}

// ResolvedSemanticCachePolicy is the per-request output. Each field is
// guaranteed populated — the resolver merges through the layers and
// always lands on plugin defaults for fields not set higher up.
type ResolvedSemanticCachePolicy struct {
	Threshold       float64
	TTL             time.Duration
	Enabled         bool
	ResolutionTrace []string // human-readable: which layer set each field
}

// PolicyResolver carries handles to all four layers + the plugin
// defaults. One instance per plugin; lookups happen per request.
type PolicyResolver struct {
	defaults  PluginDefaults
	tenants   PerTenantConfigStore
	customers PerCustomerConfigStore
	teams     PerTeamConfigStore
	vks       PerVKConfigStore
}

// Resolve walks the layers from most specific (VK) to least specific
// (plugin defaults). For each leaf field, the first non-nil pointer at
// the most specific layer wins. The trace records which layer set each
// field — useful for "why is my tenant's cache disabled?" debugging,
// and the same pattern doc 07's chain policy uses for ChainPlan.Why.
func (r *PolicyResolver) Resolve(in VKResolverInput) ResolvedSemanticCachePolicy {
	resolved := ResolvedSemanticCachePolicy{
		Threshold: r.defaults.Threshold,
		TTL:       r.defaults.TTL,
		Enabled:   r.defaults.Enabled,
	}
	threshSrc := "plugin default"
	ttlSrc := "plugin default"
	enabledSrc := "plugin default"

	// Bottom-up: tenant overrides apply on top of plugin defaults.
	if in.TenantID != "" && r.tenants != nil {
		if cfg := r.tenants.Get(in.TenantID); cfg != nil {
			if cfg.Threshold != nil {
				resolved.Threshold = *cfg.Threshold
				threshSrc = "tenant"
			}
			if cfg.TTL != nil {
				resolved.TTL = *cfg.TTL
				ttlSrc = "tenant"
			}
			if cfg.Enabled != nil {
				resolved.Enabled = *cfg.Enabled
				enabledSrc = "tenant"
			}
		}
	}

	// Customer + team overlay tenant. The choice of customer-then-team
	// vs. team-then-customer is a deployment decision; the POC picks
	// customer first because customer is usually the higher-level
	// business unit and team is the sub-unit. Either could be defended;
	// the design doc calls out the trade.
	if in.CustomerID != nil && r.customers != nil {
		if cfg := r.customers.Get(*in.CustomerID); cfg != nil {
			if cfg.Threshold != nil {
				resolved.Threshold = *cfg.Threshold
				threshSrc = "customer"
			}
			if cfg.TTL != nil {
				resolved.TTL = *cfg.TTL
				ttlSrc = "customer"
			}
			if cfg.Enabled != nil {
				resolved.Enabled = *cfg.Enabled
				enabledSrc = "customer"
			}
		}
	}
	if in.TeamID != nil && r.teams != nil {
		if cfg := r.teams.Get(*in.TeamID); cfg != nil {
			if cfg.Threshold != nil {
				resolved.Threshold = *cfg.Threshold
				threshSrc = "team"
			}
			if cfg.TTL != nil {
				resolved.TTL = *cfg.TTL
				ttlSrc = "team"
			}
			if cfg.Enabled != nil {
				resolved.Enabled = *cfg.Enabled
				enabledSrc = "team"
			}
		}
	}

	// VK-specific override is the most specific layer — wins everything.
	if in.VKID != "" && r.vks != nil {
		if cfg := r.vks.Get(in.VKID); cfg != nil {
			if cfg.Threshold != nil {
				resolved.Threshold = *cfg.Threshold
				threshSrc = "vk"
			}
			if cfg.TTL != nil {
				resolved.TTL = *cfg.TTL
				ttlSrc = "vk"
			}
			if cfg.Enabled != nil {
				resolved.Enabled = *cfg.Enabled
				enabledSrc = "vk"
			}
		}
	}

	resolved.ResolutionTrace = []string{
		"threshold: " + threshSrc,
		"ttl: " + ttlSrc,
		"enabled: " + enabledSrc,
	}
	return resolved
}

// =============================================================================
// In-memory store impls for the POC (production: configstore-backed
// adapters, same pattern as doc 09's tenant config store)
// =============================================================================

type inMemPerTenantStore struct {
	mu sync.RWMutex
	m  map[string]*TenantSemanticCacheConfig
}

func newInMemPerTenantStore() *inMemPerTenantStore {
	return &inMemPerTenantStore{m: map[string]*TenantSemanticCacheConfig{}}
}

func (s *inMemPerTenantStore) Get(id string) *TenantSemanticCacheConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[id]
}

func (s *inMemPerTenantStore) Set(id string, cfg *TenantSemanticCacheConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = cfg
}

// Customer / team / VK stores are the same shape — the POC defines
// shorter aliases to keep the test body legible.
type inMemPerCustomerStore = inMemPerTenantStore
type inMemPerTeamStore = inMemPerTenantStore
type inMemPerVKStore = inMemPerTenantStore

func newInMemPerCustomerStore() *inMemPerCustomerStore { return newInMemPerTenantStore() }
func newInMemPerTeamStore() *inMemPerTeamStore         { return newInMemPerTenantStore() }
func newInMemPerVKStore() *inMemPerVKStore             { return newInMemPerTenantStore() }

func newResolver(defaults PluginDefaults) (*PolicyResolver, *inMemPerTenantStore, *inMemPerCustomerStore, *inMemPerTeamStore, *inMemPerVKStore) {
	tStore := newInMemPerTenantStore()
	cStore := newInMemPerCustomerStore()
	teamStore := newInMemPerTeamStore()
	vkStore := newInMemPerVKStore()
	return &PolicyResolver{
		defaults:  defaults,
		tenants:   tStore,
		customers: cStore,
		teams:     teamStore,
		vks:       vkStore,
	}, tStore, cStore, teamStore, vkStore
}

func strPtr(s string) *string { return &s }

// =============================================================================
// Tests
// =============================================================================

var pluginDefaults = PluginDefaults{
	Threshold: 0.5,
	TTL:       1 * time.Hour,
	Enabled:   true,
}

// TestHierarchical_PluginDefaultsOnly — OSS regression guard. A
// request with no tenant, no VK, no customer/team → plugin defaults
// reach the call site verbatim. Trace says "plugin default" for every
// field.
func TestHierarchical_PluginDefaultsOnly(t *testing.T) {
	resolver, _, _, _, _ := newResolver(pluginDefaults)
	got := resolver.Resolve(VKResolverInput{})
	require.Equal(t, pluginDefaults.Threshold, got.Threshold)
	require.Equal(t, pluginDefaults.TTL, got.TTL)
	require.Equal(t, pluginDefaults.Enabled, got.Enabled)
	for _, line := range got.ResolutionTrace {
		require.Contains(t, line, "plugin default")
	}
}

// TestHierarchical_TenantOverridesDefaults — partial override: tenant
// sets ONLY Threshold, others stay at plugin defaults.
func TestHierarchical_TenantOverridesDefaults(t *testing.T) {
	resolver, tStore, _, _, _ := newResolver(pluginDefaults)
	tStore.Set("acme", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.10)})

	got := resolver.Resolve(VKResolverInput{TenantID: "acme"})
	require.Equal(t, 0.10, got.Threshold)
	require.Equal(t, pluginDefaults.TTL, got.TTL,
		"TTL not set at tenant layer → plugin default reaches the call site")
	require.Equal(t, pluginDefaults.Enabled, got.Enabled)
	require.Contains(t, got.ResolutionTrace[0], "threshold: tenant")
	require.Contains(t, got.ResolutionTrace[1], "ttl: plugin default")
}

// TestHierarchical_CustomerOverridesTenant — customer layer wins for
// the fields it sets; tenant layer still wins for the fields customer
// doesn't touch; plugin default for the rest.
func TestHierarchical_CustomerOverridesTenant(t *testing.T) {
	resolver, tStore, cStore, _, _ := newResolver(pluginDefaults)
	tStore.Set("acme", &TenantSemanticCacheConfig{
		Threshold: floatPtrPOC(0.30),
		TTL:       durationPtrPOC(30 * time.Minute),
	})
	cStore.Set("cust-public-affairs", &TenantSemanticCacheConfig{
		// Public-affairs customer wants tighter PII control: shorter TTL.
		// Inherits tenant's Threshold.
		TTL: durationPtrPOC(5 * time.Minute),
	})

	got := resolver.Resolve(VKResolverInput{
		TenantID:   "acme",
		CustomerID: strPtr("cust-public-affairs"),
	})
	require.Equal(t, 0.30, got.Threshold, "Threshold falls through to tenant (customer didn't set)")
	require.Equal(t, 5*time.Minute, got.TTL, "TTL set at customer wins over tenant")
	require.Contains(t, got.ResolutionTrace[0], "threshold: tenant")
	require.Contains(t, got.ResolutionTrace[1], "ttl: customer")
}

// TestHierarchical_VKOverridesEverything — VK-specific config trumps
// the entire chain.
func TestHierarchical_VKOverridesEverything(t *testing.T) {
	resolver, tStore, cStore, teamStore, vkStore := newResolver(pluginDefaults)
	tStore.Set("acme", &TenantSemanticCacheConfig{
		Threshold: floatPtrPOC(0.30),
		TTL:       durationPtrPOC(1 * time.Hour),
	})
	cStore.Set("cust-1", &TenantSemanticCacheConfig{TTL: durationPtrPOC(10 * time.Minute)})
	teamStore.Set("team-fast", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.10)})
	vkStore.Set("vk-special", &TenantSemanticCacheConfig{
		Threshold: floatPtrPOC(0.99), // matches everything
		TTL:       durationPtrPOC(7 * 24 * time.Hour),
		Enabled:   boolPtrPOC(true),
	})

	got := resolver.Resolve(VKResolverInput{
		TenantID:   "acme",
		CustomerID: strPtr("cust-1"),
		TeamID:     strPtr("team-fast"),
		VKID:       "vk-special",
	})
	require.Equal(t, 0.99, got.Threshold)
	require.Equal(t, 7*24*time.Hour, got.TTL)
	require.True(t, got.Enabled)
	for _, line := range got.ResolutionTrace {
		require.Contains(t, line, ": vk", "VK-set fields should trace to vk; got %q", line)
	}
}

// TestHierarchical_PartialOverridesCompose — the headline composition
// test. Each layer sets EXACTLY ONE field. The resolved policy carries
// one field from each layer plus plugin default for the leftover.
//
//   - Tenant sets Enabled
//   - Customer sets TTL
//   - Team sets Threshold... and then VK sets a different Threshold,
//     overriding team
//
// So the expected trace: tenant for Enabled, customer for TTL, vk
// for Threshold. Plugin default reaches nothing.
func TestHierarchical_PartialOverridesCompose(t *testing.T) {
	resolver, tStore, cStore, teamStore, vkStore := newResolver(pluginDefaults)
	tStore.Set("acme", &TenantSemanticCacheConfig{Enabled: boolPtrPOC(true)})
	cStore.Set("cust-1", &TenantSemanticCacheConfig{TTL: durationPtrPOC(15 * time.Minute)})
	teamStore.Set("team-x", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.40)})
	vkStore.Set("vk-tight", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.05)})

	got := resolver.Resolve(VKResolverInput{
		TenantID:   "acme",
		CustomerID: strPtr("cust-1"),
		TeamID:     strPtr("team-x"),
		VKID:       "vk-tight",
	})
	require.Equal(t, 0.05, got.Threshold)
	require.Equal(t, 15*time.Minute, got.TTL)
	require.True(t, got.Enabled)
	require.Equal(t, []string{
		"threshold: vk",
		"ttl: customer",
		"enabled: tenant",
	}, got.ResolutionTrace, "trace shows which layer won each field")
}

// TestHierarchical_VKDisableCascades — security-relevant case: VK
// explicitly sets Enabled=false, overriding tenant/customer
// preferences. Models a single-VK opt-out for a high-sensitivity
// integration that the rest of the customer's traffic doesn't need.
func TestHierarchical_VKDisableCascades(t *testing.T) {
	resolver, tStore, cStore, _, vkStore := newResolver(pluginDefaults)
	tStore.Set("acme", &TenantSemanticCacheConfig{Enabled: boolPtrPOC(true)})
	cStore.Set("cust-1", &TenantSemanticCacheConfig{Enabled: boolPtrPOC(true)})
	vkStore.Set("vk-secret-integration", &TenantSemanticCacheConfig{Enabled: boolPtrPOC(false)})

	got := resolver.Resolve(VKResolverInput{
		TenantID:   "acme",
		CustomerID: strPtr("cust-1"),
		VKID:       "vk-secret-integration",
	})
	require.False(t, got.Enabled, "VK explicit Enabled=false must win")
	require.Contains(t, got.ResolutionTrace[2], "enabled: vk")
}

// TestHierarchical_NoTenantStillResolvesPluginDefaults — OSS path with
// VK lookups missing. The resolver must not NPE on nil stores or
// missing rows; it falls back cleanly to plugin defaults.
func TestHierarchical_NoTenantStillResolvesPluginDefaults(t *testing.T) {
	resolver, _, _, _, _ := newResolver(pluginDefaults)
	// Empty input, but VKID set to something the store doesn't know.
	got := resolver.Resolve(VKResolverInput{VKID: "vk-unknown"})
	require.Equal(t, pluginDefaults.Threshold, got.Threshold)
	require.Equal(t, pluginDefaults.TTL, got.TTL)
	require.Equal(t, pluginDefaults.Enabled, got.Enabled)
}

// TestHierarchical_ConcurrencySafe is the race-detector canary. The
// resolver must handle concurrent Resolve + concurrent Set on any of
// the four stores. Production wires these stores against the
// configstore cache; the race-detector run here catches map-mutation
// bugs.
func TestHierarchical_ConcurrencySafe(t *testing.T) {
	resolver, tStore, cStore, teamStore, vkStore := newResolver(pluginDefaults)
	tStore.Set("acme", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.3)})
	cStore.Set("c1", &TenantSemanticCacheConfig{TTL: durationPtrPOC(10 * time.Minute)})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolver.Resolve(VKResolverInput{
				TenantID:   "acme",
				CustomerID: strPtr("c1"),
				VKID:       "vk-x",
			})
		}()
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := float64(i) / 10.0
			tStore.Set("acme", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(f)})
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			teamStore.Set("team-x", &TenantSemanticCacheConfig{TTL: durationPtrPOC(time.Duration(i) * time.Minute)})
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			vkStore.Set("vk-x", &TenantSemanticCacheConfig{Enabled: boolPtrPOC(true)})
		}()
	}
	wg.Wait()
}

// =============================================================================
// Composition with namespace + the doc 10 knob resolvers — confirms
// that the hierarchical resolver slots in CLEANLY beside the namespace
// work without changing anything about namespace resolution.
// =============================================================================

// TestHierarchical_NamespacePlusHierarchy — request flow:
// (1) resolveNamespace(ctx) picks the tenant's physical namespace,
// (2) policyResolver.Resolve picks the effective leaf config,
// (3) the per-request decision uses (namespace, threshold, ttl, enabled).
//
// Confirms namespace selection is orthogonal to leaf-config resolution.
// Tenant namespace stays "<base>_<tenant>" even when the VK config
// overrides Threshold/TTL/Enabled. The two axes compose.
func TestHierarchical_NamespacePlusHierarchy(t *testing.T) {
	resolver, tStore, _, _, vkStore := newResolver(pluginDefaults)
	tStore.Set("acme", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.30)})
	vkStore.Set("vk-tight", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.05)})

	ctx := newCtxWithTenant(t, "acme")
	ns := resolveNamespace(ctx, baseNamespace)
	policy := resolver.Resolve(VKResolverInput{TenantID: "acme", VKID: "vk-tight"})

	require.Equal(t, baseNamespace+"_acme", ns,
		"namespace is tenant-derived, independent of which VK config wins")
	require.Equal(t, 0.05, policy.Threshold, "VK threshold wins over tenant")
	require.Equal(t, "threshold: vk", policy.ResolutionTrace[0])

	// And the doc 10 knob resolvers (resolveThreshold, etc.) consume a
	// ResolvedSemanticCachePolicy in production rather than reading
	// straight from a per-tenant store. Modeled here by feeding the
	// resolved policy back into the recording store as the threshold
	// it'd use for GetNearest.
	store := newRecordingStore()
	require.NoError(t, store.Add(context.Background(), ns, "vk-tight:close",
		[]float32{0.999, 0.01, 0, 0}, nil))
	require.NoError(t, store.Add(context.Background(), ns, "vk-tight:far",
		[]float32{0.7, 0.7, 0, 0}, nil))
	hits, err := store.GetNearest(context.Background(), ns,
		[]float32{1, 0, 0, 0}, nil, nil, policy.Threshold, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1,
		"resolved threshold (0.05 from VK) gives only the close vector")
}
