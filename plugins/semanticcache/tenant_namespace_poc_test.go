package semanticcache

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
	"github.com/stretchr/testify/require"
)

// Tenant-scoped semantic-cache NAMESPACE POC — see
// docs/multi-tenant-f5/10-semantic-cache-namespace.md.
//
// patch15 prefixed the per-tenant cache KEY ('<tenant>:<base>'). That
// keeps direct hash lookups tenant-isolated, but the cache entries all
// still live in ONE physical vector store namespace
// (config.VectorStoreNamespace, default 'BifrostSemanticCachePlugin').
// Two consequences patch15 did NOT close:
//
//   1. ANN search runs across the WHOLE namespace. Hyperplane
//      partitioning (HNSW, IVF, etc.) builds one index spanning all
//      tenants' embeddings — tenant A's vectors affect the graph
//      tenant B's queries traverse. Loose similarity thresholds can
//      return cross-tenant matches even though the cache KEYS are
//      distinct, because GetNearest filters by vector distance, not
//      by the cache_key property.
//
//   2. Eviction/quota is per-namespace at the backend. Today there's
//      no way to "clear all of tenant A's cache" without iterating
//      and matching the key prefix. With per-tenant namespaces,
//      `store.DeleteNamespace(tenantNamespace)` is the operation.
//
// This POC promotes the namespace string to a per-tenant value via the
// same pattern patch15 used for cache keys (read tenant from ctx, fall
// back to the configured base namespace when absent).
//
// What the four tests prove:
//   1. Same cache_key from two tenants → entries land in DIFFERENT
//      physical namespaces in the vector store. The store sees calls
//      to Add(ns="<base>:acme",  id="acme:default-key", ...) and
//      Add(ns="<base>:globex",id="globex:default-key", ...).
//   2. GetNearest scoped to acme's namespace returns ONLY acme's
//      entries — even when the vector is more similar to globex's
//      entry. (Hyperplane partitioning concern made explicit.)
//   3. No-tenant request → falls back to base namespace. Drop-in
//      compatibility with OSS single-tenant deployments.
//   4. DeleteNamespace clears just one tenant's cache; the others'
//      entries remain. Eviction is now per-tenant primitive at the
//      backend level.

// =============================================================================
// resolveNamespace — the proposed wiring point. Same pattern as
// patch15's resolveCacheKey: read the tenant id from BifrostContext,
// derive the per-tenant namespace, fall back to the base namespace when
// absent.
// =============================================================================

// tenantNamespaceSep is the separator used to derive a tenant namespace
// from the base namespace. Picked to avoid collisions with vector-store
// naming restrictions: Weaviate class names must start with [A-Z] and
// contain only [A-Za-z0-9], Qdrant collections allow [A-Za-z0-9_-], and
// Pinecone namespaces are arbitrary strings. Underscore is the safest
// universal choice — patch16 uses `_` for the same reason in MCP
// client names.
const tenantNamespaceSep = "_"

// resolveNamespace returns the namespace the cache should use for THIS
// request. If the BifrostContext carries a tenant id under the f5xc
// tenant key, the namespace is '<baseNamespace><sep><tenantID>'. Otherwise
// (OSS single-tenant path) the base namespace is returned unchanged.
//
// In production this lives next to resolveCacheKey on *Plugin so the
// per-tenant lookup is centralized. For the POC it's a free function so
// the test can exercise it without instantiating the full plugin.
func resolveNamespace(ctx *schemas.BifrostContext, baseNamespace string) string {
	if ctx == nil {
		return baseNamespace
	}
	v := ctx.Value(tenantContextKey)
	if v == nil {
		return baseNamespace
	}
	tid, ok := v.(string)
	if !ok || tid == "" {
		return baseNamespace
	}
	return baseNamespace + tenantNamespaceSep + tid
}

// =============================================================================
// recordingStore is the smallest VectorStore mock that lets us observe
// (namespace, id) tuples. Add stamps the entry against the namespace;
// GetNearest scopes the search to one namespace only — which is what
// every real backend already does (namespace ≈ collection/class/index).
// =============================================================================

type recordingEntry struct {
	namespace string
	id        string
	embedding []float32
	metadata  map[string]any
}

type recordingStore struct {
	mu      sync.RWMutex
	entries []recordingEntry
}

func newRecordingStore() *recordingStore { return &recordingStore{} }

func (s *recordingStore) Ping(context.Context) error { return nil }

func (s *recordingStore) CreateNamespace(context.Context, string, int, map[string]vectorstore.VectorStoreProperties) error {
	return nil
}

func (s *recordingStore) DeleteNamespace(_ context.Context, namespace string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.entries[:0]
	for _, e := range s.entries {
		if e.namespace != namespace {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return nil
}

func (s *recordingStore) Add(_ context.Context, namespace, id string, embedding []float32, metadata map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, recordingEntry{
		namespace: namespace,
		id:        id,
		embedding: embedding,
		metadata:  metadata,
	})
	return nil
}

func (s *recordingStore) GetNearest(_ context.Context, namespace string, vector []float32, _ []vectorstore.Query, _ []string, threshold float64, limit int64) ([]vectorstore.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type scored struct {
		entry recordingEntry
		dist  float64
	}
	var hits []scored
	for _, e := range s.entries {
		// The CRITICAL invariant: GetNearest only considers entries in
		// the requested namespace — matching every real backend
		// (Weaviate class scope, Qdrant collection scope, etc.). This
		// is exactly the property the POC depends on for isolation.
		if e.namespace != namespace {
			continue
		}
		d := cosineDistance(vector, e.embedding)
		if d <= threshold || threshold == 0 {
			hits = append(hits, scored{entry: e, dist: d})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].dist < hits[j].dist })
	if int64(len(hits)) > limit && limit > 0 {
		hits = hits[:limit]
	}
	out := make([]vectorstore.SearchResult, 0, len(hits))
	for _, h := range hits {
		score := 1.0 - h.dist
		out = append(out, vectorstore.SearchResult{
			ID:         h.entry.id,
			Properties: h.entry.metadata,
			Score:      &score,
		})
	}
	return out, nil
}

// Unused methods required by the VectorStore interface; deliberately
// minimal — the POC only exercises Add, GetNearest, DeleteNamespace.
func (s *recordingStore) GetChunk(context.Context, string, string) (vectorstore.SearchResult, error) {
	return vectorstore.SearchResult{}, vectorstore.ErrNotFound
}
func (s *recordingStore) GetChunks(context.Context, string, []string) ([]vectorstore.SearchResult, error) {
	return nil, vectorstore.ErrNotSupported
}
func (s *recordingStore) GetAll(context.Context, string, []vectorstore.Query, []string, *string, int64) ([]vectorstore.SearchResult, *string, error) {
	return nil, nil, vectorstore.ErrNotSupported
}
func (s *recordingStore) RequiresVectors() bool                   { return true }
func (s *recordingStore) Delete(context.Context, string, string) error { return nil }
func (s *recordingStore) DeleteAll(context.Context, string, []vectorstore.Query) ([]vectorstore.DeleteResult, error) {
	return nil, vectorstore.ErrNotSupported
}
func (s *recordingStore) Close(context.Context, string) error { return nil }

// =============================================================================
// Test helpers
// =============================================================================

const baseNamespace = "BifrostSemanticCachePlugin"

func newCtxWithTenant(t *testing.T, tenant string) *schemas.BifrostContext {
	t.Helper()
	c := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if tenant != "" {
		c = c.WithValue(tenantContextKey, tenant)
	}
	return c
}

// cosineDistance returns 1 - cosine_similarity. Both vectors must be the
// same length; the POC uses 4-D vectors for simplicity. Returns 1.0
// when either vector is zero-length (treat as maximally distant — the
// POC never produces zero vectors).
func cosineDistance(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 1.0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1.0
	}
	cos := dot / (sqrt(na) * sqrt(nb))
	return 1.0 - cos
}

func sqrt(x float64) float64 {
	// Tiny Newton-Raphson — keeps the test self-contained without
	// importing math just for this helper.
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 10; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// =============================================================================
// Tests
// =============================================================================

// TestNamespacePOC_TwoTenantsAddToDistinctNamespaces is the headline
// isolation property: same cache key from two tenants → different
// physical namespaces in the store. patch15's key prefix accomplished
// half of this (different ids); the namespace upgrade accomplishes the
// other half (different ANN graphs).
func TestNamespacePOC_TwoTenantsAddToDistinctNamespaces(t *testing.T) {
	store := newRecordingStore()
	ctxA := newCtxWithTenant(t, "acme")
	ctxB := newCtxWithTenant(t, "globex")

	require.NoError(t, store.Add(context.Background(),
		resolveNamespace(ctxA, baseNamespace), "acme:default-key",
		[]float32{1, 0, 0, 0}, map[string]any{"prompt": "hello"}))

	require.NoError(t, store.Add(context.Background(),
		resolveNamespace(ctxB, baseNamespace), "globex:default-key",
		[]float32{0, 1, 0, 0}, map[string]any{"prompt": "hi"}))

	store.mu.RLock()
	defer store.mu.RUnlock()
	require.Len(t, store.entries, 2)
	nsByID := map[string]string{}
	for _, e := range store.entries {
		nsByID[e.id] = e.namespace
	}
	require.Equal(t, baseNamespace+"_acme", nsByID["acme:default-key"])
	require.Equal(t, baseNamespace+"_globex", nsByID["globex:default-key"])
	require.NotEqual(t, nsByID["acme:default-key"], nsByID["globex:default-key"],
		"two tenants must land in two distinct vector-store namespaces")
}

// TestNamespacePOC_GetNearestScopedToTenant is the ANN-isolation
// property: with a loose threshold that WOULD match the other tenant's
// entry via vector distance, the namespace scope confines the result.
// This is the test patch15's key-prefix model can't pass on a real ANN
// backend.
func TestNamespacePOC_GetNearestScopedToTenant(t *testing.T) {
	store := newRecordingStore()

	// Acme's only entry: vector [1, 0, 0, 0]
	ctxA := newCtxWithTenant(t, "acme")
	require.NoError(t, store.Add(context.Background(),
		resolveNamespace(ctxA, baseNamespace), "acme:greeting",
		[]float32{1, 0, 0, 0}, map[string]any{"prompt": "hello"}))

	// Globex's only entry: very close vector [0.95, 0.05, 0, 0] — would
	// match acme's at almost any reasonable threshold if they shared
	// a namespace.
	ctxB := newCtxWithTenant(t, "globex")
	require.NoError(t, store.Add(context.Background(),
		resolveNamespace(ctxB, baseNamespace), "globex:greeting",
		[]float32{0.95, 0.05, 0, 0}, map[string]any{"prompt": "hi"}))

	// Acme queries with their own vector. Threshold = 1.0 is "match
	// anything in this namespace at any distance" — the absolute
	// worst-case for cross-tenant pollution.
	hits, err := store.GetNearest(context.Background(),
		resolveNamespace(ctxA, baseNamespace),
		[]float32{1, 0, 0, 0}, nil, nil, 1.0, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "even with threshold=1.0, only acme's entry is in acme's namespace")
	require.Equal(t, "acme:greeting", hits[0].ID)

	// Sanity: same query against globex's namespace returns globex's entry.
	hits, err = store.GetNearest(context.Background(),
		resolveNamespace(ctxB, baseNamespace),
		[]float32{1, 0, 0, 0}, nil, nil, 1.0, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "globex:greeting", hits[0].ID)
}

// TestNamespacePOC_NoTenantFallsBackToBase confirms OSS single-tenant
// compatibility. A request without an f5xc tenant on ctx lands in the
// base namespace — bit-identical to today's behavior. The regression
// guard for any deployment that doesn't speak multi-tenant.
func TestNamespacePOC_NoTenantFallsBackToBase(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ns := resolveNamespace(ctx, baseNamespace)
	require.Equal(t, baseNamespace, ns, "no tenant on ctx → base namespace unchanged (OSS path)")

	// Also nil ctx must be safe — guard for any call site we miss.
	require.Equal(t, baseNamespace, resolveNamespace(nil, baseNamespace))
}

// =============================================================================
// TenantSemanticCacheConfig — same shape as doc 09's TenantTelemetryConfig.
// Per-tenant knobs that the resolver functions below merge against the
// plugin's base config to produce the final per-request value.
//
// Pointer-typed numerics for the same reason doc 09's config does: the
// zero value 0.0 is a valid setting (threshold=0 means "match anything";
// TTL=0 means "no expiry") and we need a third state ("not set, use
// plugin default"). nil = "field not configured for this tenant."
// =============================================================================

type TenantSemanticCacheConfig struct {
	// Threshold ∈ [0,1] — cosine-distance ceiling. Lower = stricter
	// match required to count as a hit. nil = use plugin default.
	Threshold *float64
	// TTL — how long entries live before they're stale. nil = use
	// plugin default. Zero is a valid explicit "no expiry."
	TTL *time.Duration
	// Enabled — false explicitly bypasses the cache for this tenant
	// (every request becomes a MISS, nothing is added). nil = enabled.
	Enabled *bool
}

// floatPtr / durationPtr / boolPtr — test ergonomics, same as doc 09's
// helper. Production code reads these from configstore rows where the
// pointer-vs-nil distinction comes from NULL columns.
func floatPtrPOC(f float64) *float64           { return &f }
func durationPtrPOC(d time.Duration) *time.Duration { return &d }
func boolPtrPOC(b bool) *bool                  { return &b }

// TenantCacheConfigStore abstracts the source. Production:
// configstore-backed cache. Tests: inMemoryTenantCacheConfigStore.
type TenantCacheConfigStore interface {
	Get(tenantID string) *TenantSemanticCacheConfig
}

type inMemoryTenantCacheConfigStore struct {
	mu      sync.RWMutex
	configs map[string]*TenantSemanticCacheConfig
}

func newInMemoryTenantCacheConfigStore() *inMemoryTenantCacheConfigStore {
	return &inMemoryTenantCacheConfigStore{configs: map[string]*TenantSemanticCacheConfig{}}
}

func (s *inMemoryTenantCacheConfigStore) Get(tid string) *TenantSemanticCacheConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configs[tid]
}

func (s *inMemoryTenantCacheConfigStore) Set(tid string, cfg *TenantSemanticCacheConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[tid] = cfg
}

// tenantIDFromCtx is the centralized read of the f5xc tenant key. Three
// resolvers below all use it; the helper keeps them honest.
func tenantIDFromCtx(ctx *schemas.BifrostContext) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(tenantContextKey)
	if v == nil {
		return ""
	}
	tid, _ := v.(string)
	return tid
}

// resolveThreshold — proposed wiring: plugin.config.Threshold is the
// process-global default; per-tenant config can tighten or loosen it.
func resolveThreshold(ctx *schemas.BifrostContext, store TenantCacheConfigStore, baseThreshold float64) float64 {
	tid := tenantIDFromCtx(ctx)
	if tid == "" || store == nil {
		return baseThreshold
	}
	if cfg := store.Get(tid); cfg != nil && cfg.Threshold != nil {
		return *cfg.Threshold
	}
	return baseThreshold
}

// resolveTTL — similarly per-tenant; sensitive tenants want short
// retention, compliance tenants want long.
func resolveTTL(ctx *schemas.BifrostContext, store TenantCacheConfigStore, baseTTL time.Duration) time.Duration {
	tid := tenantIDFromCtx(ctx)
	if tid == "" || store == nil {
		return baseTTL
	}
	if cfg := store.Get(tid); cfg != nil && cfg.TTL != nil {
		return *cfg.TTL
	}
	return baseTTL
}

// isCacheEnabled — false ONLY when the tenant has an explicit Enabled=false.
// Default behavior (no config, no Enabled set) is enabled.
func isCacheEnabled(ctx *schemas.BifrostContext, store TenantCacheConfigStore) bool {
	tid := tenantIDFromCtx(ctx)
	if tid == "" || store == nil {
		return true
	}
	if cfg := store.Get(tid); cfg != nil && cfg.Enabled != nil {
		return *cfg.Enabled
	}
	return true
}

// TestNamespacePOC_PerTenantThreshold — strict vs. loose threshold per
// tenant. Same query vector against same-namespace data produces
// different hit counts because the per-tenant threshold is applied.
//
// The default plugin threshold is 0.5 (cosine distance — entries
// within 0.5 distance are hits). Acme overrides to 0.05 (very strict);
// globex overrides to 0.99 (essentially "match anything").
func TestNamespacePOC_PerTenantThreshold(t *testing.T) {
	store := newRecordingStore()
	cfgStore := newInMemoryTenantCacheConfigStore()
	const baseThreshold = 0.5

	cfgStore.Set("acme", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.05)})
	cfgStore.Set("globex", &TenantSemanticCacheConfig{Threshold: floatPtrPOC(0.99)})

	// Seed each tenant's namespace with two entries:
	//   "close":   vector very similar to the query (distance ~0.01)
	//   "far":     vector quite different (distance ~0.45)
	for _, tid := range []string{"acme", "globex"} {
		ctx := newCtxWithTenant(t, tid)
		ns := resolveNamespace(ctx, baseNamespace)
		require.NoError(t, store.Add(context.Background(), ns, tid+":close",
			[]float32{0.999, 0.01, 0, 0}, nil))
		require.NoError(t, store.Add(context.Background(), ns, tid+":far",
			[]float32{0.7, 0.7, 0, 0}, nil))
	}

	query := []float32{1, 0, 0, 0}

	// Acme — strict (0.05) — should only match "close" (dist ~0.0001),
	// not "far" (dist ~0.5).
	ctxA := newCtxWithTenant(t, "acme")
	threshA := resolveThreshold(ctxA, cfgStore, baseThreshold)
	require.Equal(t, 0.05, threshA)
	hitsA, err := store.GetNearest(context.Background(),
		resolveNamespace(ctxA, baseNamespace), query, nil, nil, threshA, 10)
	require.NoError(t, err)
	require.Len(t, hitsA, 1, "strict acme threshold should only match the close vector")
	require.Equal(t, "acme:close", hitsA[0].ID)

	// Globex — loose (0.99) — should match both.
	ctxB := newCtxWithTenant(t, "globex")
	threshB := resolveThreshold(ctxB, cfgStore, baseThreshold)
	require.Equal(t, 0.99, threshB)
	hitsB, err := store.GetNearest(context.Background(),
		resolveNamespace(ctxB, baseNamespace), query, nil, nil, threshB, 10)
	require.NoError(t, err)
	require.Len(t, hitsB, 2, "loose globex threshold should match both vectors")
}

// TestNamespacePOC_PerTenantTTL — resolver returns the per-tenant TTL
// when set, otherwise base. Storage-level TTL enforcement is the
// backend's responsibility (Weaviate has class-level TTL; Redis has
// EXPIRE; Pinecone enforces at scrape time). The plugin's job is to
// surface the right number for the right tenant.
func TestNamespacePOC_PerTenantTTL(t *testing.T) {
	cfgStore := newInMemoryTenantCacheConfigStore()
	const baseTTL = 1 * time.Hour
	cfgStore.Set("sensitive", &TenantSemanticCacheConfig{TTL: durationPtrPOC(5 * time.Minute)})
	cfgStore.Set("compliance", &TenantSemanticCacheConfig{TTL: durationPtrPOC(30 * 24 * time.Hour)})

	require.Equal(t, baseTTL, resolveTTL(nil, cfgStore, baseTTL),
		"nil ctx falls back to base TTL (OSS regression guard)")
	require.Equal(t, baseTTL, resolveTTL(newCtxWithTenant(t, ""), cfgStore, baseTTL),
		"empty tenant id falls back to base TTL")
	require.Equal(t, baseTTL, resolveTTL(newCtxWithTenant(t, "unknown"), cfgStore, baseTTL),
		"unknown tenant (no config row) gets base TTL")

	require.Equal(t, 5*time.Minute, resolveTTL(newCtxWithTenant(t, "sensitive"), cfgStore, baseTTL),
		"sensitive tenant: shortened to 5m")
	require.Equal(t, 30*24*time.Hour, resolveTTL(newCtxWithTenant(t, "compliance"), cfgStore, baseTTL),
		"compliance tenant: extended to 30d")

	// TTL=0 must be respected as an EXPLICIT "no expiry" — not collapsed
	// back to the default.
	cfgStore.Set("forever", &TenantSemanticCacheConfig{TTL: durationPtrPOC(0)})
	require.Equal(t, time.Duration(0), resolveTTL(newCtxWithTenant(t, "forever"), cfgStore, baseTTL),
		"explicit TTL=0 means no expiry; must not fall back to base")
}

// TestNamespacePOC_PerTenantDisable — an explicit opt-out tenant gets
// no cache writes or reads. Used for tenants who don't want their
// prompts retained in any form (compliance), or for tenants whose
// traffic shape makes the cache a net loss (low repeat rate).
func TestNamespacePOC_PerTenantDisable(t *testing.T) {
	store := newRecordingStore()
	cfgStore := newInMemoryTenantCacheConfigStore()
	cfgStore.Set("noprivacy", &TenantSemanticCacheConfig{Enabled: boolPtrPOC(false)})

	// Default (no config): enabled.
	require.True(t, isCacheEnabled(newCtxWithTenant(t, "acme"), cfgStore))
	// Explicit false: disabled.
	require.False(t, isCacheEnabled(newCtxWithTenant(t, "noprivacy"), cfgStore))
	// Explicit true (paranoid double-set): enabled.
	cfgStore.Set("paranoid", &TenantSemanticCacheConfig{Enabled: boolPtrPOC(true)})
	require.True(t, isCacheEnabled(newCtxWithTenant(t, "paranoid"), cfgStore))

	// A would-be cache write for the disabled tenant short-circuits
	// before touching the store. Model what the production wiring
	// would look like.
	maybeAdd := func(ctx *schemas.BifrostContext, ns, id string, v []float32) {
		if !isCacheEnabled(ctx, cfgStore) {
			return
		}
		_ = store.Add(context.Background(), ns, id, v, nil)
	}
	maybeAdd(newCtxWithTenant(t, "acme"), resolveNamespace(newCtxWithTenant(t, "acme"), baseNamespace),
		"acme:1", []float32{1, 0, 0, 0})
	maybeAdd(newCtxWithTenant(t, "noprivacy"), resolveNamespace(newCtxWithTenant(t, "noprivacy"), baseNamespace),
		"noprivacy:1", []float32{1, 0, 0, 0})

	store.mu.RLock()
	defer store.mu.RUnlock()
	require.Len(t, store.entries, 1, "only acme's entry should have landed; noprivacy short-circuited")
	require.Equal(t, "acme:1", store.entries[0].id)
}

// TestNamespacePOC_AllKnobsCompose — exercise namespace + threshold +
// TTL + enabled in one flow. Models the production wiring: every
// request's cache decision is the composition of (namespace,
// threshold, TTL, enabled) all resolved from ctx + tenant config.
//
// Three tenants share the same workload (same query vector against
// equivalent seed data) but produce three observably different
// behaviors because each knob is per-tenant.
func TestNamespacePOC_AllKnobsCompose(t *testing.T) {
	store := newRecordingStore()
	cfgStore := newInMemoryTenantCacheConfigStore()
	const (
		baseThreshold = 0.5
		baseTTL       = 1 * time.Hour
	)
	cfgStore.Set("strict", &TenantSemanticCacheConfig{
		Threshold: floatPtrPOC(0.05),
		TTL:       durationPtrPOC(5 * time.Minute),
	})
	cfgStore.Set("loose", &TenantSemanticCacheConfig{
		Threshold: floatPtrPOC(0.99),
		TTL:       durationPtrPOC(24 * time.Hour),
	})
	cfgStore.Set("disabled", &TenantSemanticCacheConfig{
		Enabled: boolPtrPOC(false),
	})

	seed := func(tid string) {
		ctx := newCtxWithTenant(t, tid)
		if !isCacheEnabled(ctx, cfgStore) {
			return
		}
		ns := resolveNamespace(ctx, baseNamespace)
		require.NoError(t, store.Add(context.Background(), ns, tid+":close",
			[]float32{0.999, 0.01, 0, 0}, nil))
		require.NoError(t, store.Add(context.Background(), ns, tid+":far",
			[]float32{0.7, 0.7, 0, 0}, nil))
	}
	seed("strict")
	seed("loose")
	seed("disabled")

	query := []float32{1, 0, 0, 0}

	type result struct {
		tenant string
		hits   int
		ttl    time.Duration
		ns     string
	}
	resolve := func(tid string) result {
		ctx := newCtxWithTenant(t, tid)
		if !isCacheEnabled(ctx, cfgStore) {
			return result{tenant: tid, hits: 0, ns: "(cache disabled)"}
		}
		ns := resolveNamespace(ctx, baseNamespace)
		thr := resolveThreshold(ctx, cfgStore, baseThreshold)
		ttl := resolveTTL(ctx, cfgStore, baseTTL)
		hits, err := store.GetNearest(context.Background(), ns, query, nil, nil, thr, 10)
		require.NoError(t, err)
		return result{tenant: tid, hits: len(hits), ttl: ttl, ns: ns}
	}

	require.Equal(t, result{
		tenant: "strict", hits: 1, ttl: 5 * time.Minute,
		ns: baseNamespace + "_strict",
	}, resolve("strict"))
	require.Equal(t, result{
		tenant: "loose", hits: 2, ttl: 24 * time.Hour,
		ns: baseNamespace + "_loose",
	}, resolve("loose"))
	require.Equal(t, result{
		tenant: "disabled", hits: 0, ttl: 0,
		ns: "(cache disabled)",
	}, resolve("disabled"))

	// Confirm the store NEVER saw entries for the disabled tenant —
	// the seed short-circuited before any Add.
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, e := range store.entries {
		require.NotEqual(t, baseNamespace+"_disabled", e.namespace,
			"disabled tenant should have zero entries in the store; found %s", e.id)
	}
}

// TestNamespacePOC_DeleteNamespaceClearsOnlyOneTenant — eviction
// primitive. The whole reason per-tenant namespaces are operationally
// useful: "tenant deleted, drop all their cache" becomes one
// DeleteNamespace call instead of scanning every key for a prefix
// match. Verifies the boundary.
func TestNamespacePOC_DeleteNamespaceClearsOnlyOneTenant(t *testing.T) {
	store := newRecordingStore()
	tenants := []string{"acme", "globex", "umbra"}
	for _, tid := range tenants {
		require.NoError(t, store.Add(context.Background(),
			resolveNamespace(newCtxWithTenant(t, tid), baseNamespace),
			tid+":key", []float32{1, 0, 0, 0}, nil))
	}
	require.Len(t, store.entries, 3)

	require.NoError(t, store.DeleteNamespace(context.Background(),
		resolveNamespace(newCtxWithTenant(t, "globex"), baseNamespace)))

	store.mu.RLock()
	defer store.mu.RUnlock()
	require.Len(t, store.entries, 2, "only globex's entry should be gone")
	for _, e := range store.entries {
		require.NotEqual(t, baseNamespace+"_globex", e.namespace)
	}
}
