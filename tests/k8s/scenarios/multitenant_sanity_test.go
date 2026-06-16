//go:build k8s

package scenarios

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/maximhq/bifrost/tests/k8s/fixtures"
)

// TestMultiTenantSanity boots Bifrost with multi-tenancy ON
// (BIFROST_MULTI_TENANT_ENABLED=true), provisions two independent tenants
// against the in-cluster mock LLM, drives one chat per tenant, then
// deletes tenant A's virtual key and verifies:
//
//  1. tenant A's chat now 401s (proof the VK resolver cache was
//     invalidated immediately, per patch 0028 — without the
//     invalidator, the deleted VK would still resolve for up to 60s),
//  2. tenant B's chat still succeeds (proof the invalidation was
//     scoped to A's VK only).
//
// This is the smallest E2E that proves multi-tenant routing is
// end-to-end correct on a real binary in a real cluster. Streaming,
// async, MCP, multi-replica propagation, and Anthropic-shape scenarios
// each get their own follow-on test.
//
// Run with:
//
//	make test-k8s-image
//	cd tests/k8s && GOWORK=off BIFROST_K8S_SKIP_BUILD=1 \
//	    go test -tags=k8s -count=1 -timeout 30m \
//	    -run '^TestMultiTenantSanity$' -v ./scenarios/...
func TestMultiTenantSanity(t *testing.T) {
	c, stopCluster := fixtures.NewKindCluster(t, fixtures.WithKindReuse())
	defer stopCluster()

	// adminDefaultTenant = "" — this scenario only drives /api/platform/*
	// and /api/tenants/{tid}/*, never the legacy single-tenant surface,
	// so AdminAuthz has no fallback work to do.
	bf, teardown := fixtures.NewBifrostInstall(t, c, fixtures.WithMultiTenant(""))
	defer teardown()

	if !bf.MultiTenantEnabled {
		t.Fatal("BifrostInstall reports multi-tenant disabled despite WithMultiTenant — option not wired through")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	mt := bf.MTAdmin()

	// Both tenants share the same in-cluster mock LLM URL — isolation is
	// asserted at the routing/auth layer (each tenant resolves to its
	// own runtime), not by giving each tenant a different backend.
	mockURL := bf.MockLLM.InClusterURL

	tenants := []struct {
		ID, Name string
	}{
		{"acme", "Acme Industries"},
		{"globex", "Globex Corporation"},
	}

	vkByTenant := map[string]string{}
	for _, tn := range tenants {
		mt.CreateTenant(t, ctx, tn.ID, tn.Name)
		mt.CreateProvider(t, ctx, tn.ID, "openai", mockURL)
		mt.CreateProviderKey(t, ctx, tn.ID, "openai", "mock", "mock-key")
		// Same Name across tenants exercises the composite
		// (tenant_id, name) unique index on governance_virtual_keys —
		// see migrationTenantScopedVKNameUnique. With the legacy
		// single-column index this POST would 409 on the second loop.
		vkByTenant[tn.ID] = mt.CreateVirtualKey(t, ctx, tn.ID, "primary")
	}

	// Drive one inference per tenant. WaitForVKResolver handles the
	// case where the tenant runtime hasn't lazy-loaded yet — the
	// initial 401 retries inside the 30s budget rather than failing
	// the test on a cold start.
	for _, tn := range tenants {
		res := mt.WaitForVKResolver(t, ctx, vkByTenant[tn.ID], "openai", "gpt-4o-mock", 30*time.Second)
		if res.Err != nil {
			t.Fatalf("tenant %s: chat call errored: %v", tn.ID, res.Err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("tenant %s: chat call status %d: %s", tn.ID, res.StatusCode, fixtures.SnippetOf(res.Body, 240))
		}
		t.Logf("tenant %s: chat 200 OK (snippet=%s)", tn.ID, fixtures.SnippetOf(res.Body, 120))
	}

	// --- Patch-0028 assertion: deleting a VK invalidates the resolver
	// cache same-replica so the next chat 401s instead of routing.
	acmeVKs := mt.ListVirtualKeys(t, ctx, "acme")
	if len(acmeVKs) != 1 {
		t.Fatalf("acme: expected 1 VK after seed; got %d (%v)", len(acmeVKs), acmeVKs)
	}
	mt.DeleteVirtualKey(t, ctx, "acme", acmeVKs[0].ID)

	deletedRes := mt.Chat(ctx, vkByTenant["acme"], "openai", "gpt-4o-mock", "after-delete")
	if deletedRes.Err != nil {
		t.Fatalf("acme post-delete chat errored: %v", deletedRes.Err)
	}
	if deletedRes.StatusCode == http.StatusOK {
		t.Fatalf("acme post-delete chat unexpectedly 200 — resolver cache NOT invalidated (patch 0028 broken or not deployed): %s",
			fixtures.SnippetOf(deletedRes.Body, 240))
	}
	t.Logf("acme post-delete chat status=%d (expected 401/403/404) — resolver cache invalidation confirmed", deletedRes.StatusCode)

	// Globex's VK was never touched — it must still work. This rules
	// out an InvalidateAll-style overcorrection.
	stillRes := mt.Chat(ctx, vkByTenant["globex"], "openai", "gpt-4o-mock", "after-acme-delete")
	if stillRes.Err != nil {
		t.Fatalf("globex post-delete chat errored: %v", stillRes.Err)
	}
	if stillRes.StatusCode != http.StatusOK {
		t.Fatalf("globex post-acme-delete chat status %d — invalidation leaked across tenants: %s",
			stillRes.StatusCode, fixtures.SnippetOf(stillRes.Body, 240))
	}
	t.Logf("globex chat 200 OK after acme VK delete — per-VK invalidation confirmed")
}
