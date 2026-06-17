package semanticcache

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestResolveCacheKey_TenantScope locks the patch15 invariant: when the
// BifrostContext carries a non-empty tenant id under the f5xc tenant key,
// resolveCacheKey returns "<tenant>:<base>" so two tenants cannot share a
// cache bucket — even when they pick the same per-request CacheKey or share
// the same DefaultCacheKey. This is the unit-level counterpart of isolation
// test I7.
func TestResolveCacheKey_TenantScope(t *testing.T) {
	// Re-declare the typed tenant key locally so this test compiles against
	// the upstream main.go (where the const is unexported). The value MUST
	// match multitenant.BifrostContextKeyTenantID — see the comment block in
	// main.go above the const declaration.
	const tenantKey schemas.BifrostContextKey = "x-f5xc-tenant"

	newCtx := func(tenant, perRequestCacheKey string) *schemas.BifrostContext {
		c := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if perRequestCacheKey != "" {
			c = c.WithValue(CacheKey, perRequestCacheKey)
		}
		if tenant != "" {
			c = c.WithValue(tenantKey, tenant)
		}
		return c
	}

	cases := []struct {
		name             string
		tenant           string
		perRequestKey    string
		defaultCacheKey  string
		wantKey          string
		wantOK           bool
	}{
		{
			name:          "no tenant, per-request key only",
			tenant:        "",
			perRequestKey: "feature-x",
			wantKey:       "feature-x",
			wantOK:        true,
		},
		{
			name:            "no tenant, default key only",
			tenant:          "",
			defaultCacheKey: "global-default",
			wantKey:         "global-default",
			wantOK:          true,
		},
		{
			name:          "tenant + per-request key → prefixed",
			tenant:        "acme",
			perRequestKey: "feature-x",
			wantKey:       "acme:feature-x",
			wantOK:        true,
		},
		{
			name:            "tenant + default key → prefixed",
			tenant:          "acme",
			defaultCacheKey: "global-default",
			wantKey:         "acme:global-default",
			wantOK:          true,
		},
		{
			name:    "no tenant, no per-request key, no default → disabled",
			wantOK:  false,
			wantKey: "",
		},
		{
			name:    "tenant set but no key + no default → still disabled",
			tenant:  "acme",
			wantOK:  false,
			wantKey: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{config: &Config{DefaultCacheKey: tc.defaultCacheKey}}
			got, ok := p.resolveCacheKey(newCtx(tc.tenant, tc.perRequestKey))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (key=%q)", ok, tc.wantOK, got)
			}
			if got != tc.wantKey {
				t.Fatalf("key = %q, want %q", got, tc.wantKey)
			}
		})
	}
}

// TestResolveCacheKey_TenantsDoNotShareBuckets is the cross-tenant leak
// canary: two tenants picking the SAME explicit CacheKey must produce
// DIFFERENT resolved keys. Before patch15 this returned equal strings
// (= the bug). Single property; if it ever regresses, I7 also flips red.
func TestResolveCacheKey_TenantsDoNotShareBuckets(t *testing.T) {
	const tenantKey schemas.BifrostContextKey = "x-f5xc-tenant"

	p := &Plugin{config: &Config{}}
	ctxA := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline).
		WithValue(CacheKey, "shared-feature").
		WithValue(tenantKey, "tenant-a")
	ctxB := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline).
		WithValue(CacheKey, "shared-feature").
		WithValue(tenantKey, "tenant-b")

	keyA, okA := p.resolveCacheKey(ctxA)
	keyB, okB := p.resolveCacheKey(ctxB)

	if !okA || !okB {
		t.Fatalf("both ctxs should resolve a key; got okA=%v okB=%v", okA, okB)
	}
	if keyA == keyB {
		t.Fatalf("cross-tenant cache key MUST differ; both returned %q (cross-tenant leak)", keyA)
	}
	// Sanity: both keys end with the shared base.
	if !endsWith(keyA, ":shared-feature") || !endsWith(keyB, ":shared-feature") {
		t.Fatalf("expected '<tenant>:shared-feature' shape; got %q and %q", keyA, keyB)
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
