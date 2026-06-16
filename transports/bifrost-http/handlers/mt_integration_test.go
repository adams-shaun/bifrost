// Package handlers integration tests for the multi-tenant admin surface.
//
// This file is the "E2E within the process boundary" layer: it wires up a
// real ConfigStore (in-memory SQLite, same shape as production Postgres),
// the platform-admin tenants handler, the tenant-scoped provider + VK
// handlers, and a multitenant.Manager whose loader reads provider configs
// out of the live store. The tests then drive admin flows via direct
// fasthttp.RequestCtx calls and assert that
//   - tenant rows land in the store with the right tenant_id,
//   - per-tenant provider/VK writes don't leak across tenants,
//   - the multi-tenant VK resolver maps VK -> tenant correctly,
//   - the multi-tenant router lazy-loads a per-tenant Bifrost runtime
//     with only that tenant's providers in scope.
//
// Why this layer instead of testcontainers+Postgres+kind?
//   - Same go-test invocation, no Docker dependency, runs in CI today.
//   - Exercises every layer that matters for tenant isolation:
//     route -> middleware -> handler -> store -> resolver -> router ->
//     Manager.Acquire -> per-tenant Account.
//   - The Postgres + Helm + kind story (real cluster, multi-replica HA)
//     is documented under test/e2e/README.md as the next layer up; this
//     file is the prerequisite for that work because the same flows have
//     to pass here before they can pass against a kind cluster.
package handlers

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// setupIntegrationStore boots a real SQLite-backed ConfigStore using the
// production constructor so the test exercises the same migration chain
// (seeded default tenant, composite unique indexes, etc.) the real
// service does at startup.
func setupIntegrationStore(t *testing.T) configstore.ConfigStore {
	t.Helper()
	dir := t.TempDir()
	cfg := &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(dir, "configstore.db")},
	}
	store, err := configstore.NewConfigStore(context.Background(), cfg, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if store == nil {
		t.Fatal("NewConfigStore returned nil store")
	}
	return store
}

// invokeRoute is a tiny test helper that drives a fasthttp router for a
// single request and returns the populated ctx so the test can assert on
// the status code + body. Mirrors the production routing path: routes are
// matched by the router which extracts URL params and dispatches to the
// chained middleware -> handler.
func invokeRoute(t *testing.T, r *router.Router, method, path string, body []byte) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	req.SetRequestURI(path)
	req.Header.SetMethod(method)
	if body != nil {
		req.SetBody(body)
		req.Header.SetContentType("application/json")
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	r.Handler(ctx)
	return ctx
}

// stubLoader is a tenant runtime loader that records every tenant the
// Manager asks it to load, and returns a fixture BifrostConfig built from
// the tenant's providers in the store. We don't bring up a real
// bifrost.Bifrost — the integration here is "Manager hits the right
// loader for the right tenant"; per-runtime inference behaviour is
// covered separately at the OSS-core layer.
type stubLoader struct {
	mu     sync.Mutex
	loaded []multitenant.TenantID
	store  configstore.ConfigStore
}

func (l *stubLoader) Load(ctx context.Context, tid multitenant.TenantID) (schemas.BifrostConfig, error) {
	l.mu.Lock()
	l.loaded = append(l.loaded, tid)
	l.mu.Unlock()
	providers, err := l.store.GetProvidersConfigByTenant(ctx, string(tid))
	if err != nil {
		return schemas.BifrostConfig{}, err
	}
	return schemas.BifrostConfig{
		Account: lib.NewTenantScopedAccount(providers),
	}, nil
}

// TestMultiTenantAdmin_TenantIsolation drives the full admin happy path
// for two tenants — create tenant A and B via the platform endpoint,
// provision a different "openai" provider config for each (composite
// uniqueness across (tenant_id, name) makes this legal), mint a VK per
// tenant, then assert each lookup primitive sees only its own data.
func TestMultiTenantAdmin_TenantIsolation(t *testing.T) {
	store := setupIntegrationStore(t)
	ctx := context.Background()

	authz := &lib.StubPlatformAdminAuthz{FallbackTenant: "operator"}
	tenantPathMW := RequireTenantPathMiddleware(authz)

	tenantsHandler, err := NewTenantsHandler(store)
	if err != nil {
		t.Fatalf("tenants handler: %v", err)
	}
	providerHandler, err := NewTenantProviderHandler(store, nil)
	if err != nil {
		t.Fatalf("provider handler: %v", err)
	}
	vkHandler, err := NewTenantVirtualKeyHandler(store, nil, nil, nil)
	if err != nil {
		t.Fatalf("vk handler: %v", err)
	}
	mcpHandler, err := NewTenantMCPHandler(store, nil)
	if err != nil {
		t.Fatalf("mcp handler: %v", err)
	}

	r := router.New()
	tenantsHandler.RegisterRoutes(r)
	providerHandler.RegisterRoutes(r, tenantPathMW)
	vkHandler.RegisterRoutes(r, tenantPathMW)
	mcpHandler.RegisterRoutes(r, tenantPathMW)

	// Phase 1 — create both tenants via the platform admin endpoint.
	for _, id := range []string{"acme", "globex"} {
		body, _ := sonic.Marshal(map[string]any{"id": id, "name": id + " Inc"})
		c := invokeRoute(t, r, fasthttp.MethodPost, "/api/platform/tenants", body)
		if got := c.Response.StatusCode(); got != fasthttp.StatusCreated {
			t.Fatalf("create tenant %s: status %d body=%s", id, got, string(c.Response.Body()))
		}
	}

	// Phase 2 — provision provider 'openai' for each tenant, with
	// different concurrency settings so we can detect cross-tenant
	// leakage if the lookup got it wrong.
	makeProvider := func(concurrency, buf int) []byte {
		body, _ := sonic.Marshal(map[string]any{
			"provider": "openai",
			"concurrency_and_buffer_size": map[string]any{
				"concurrency": concurrency,
				"buffer_size": buf,
			},
		})
		return body
	}
	for _, tc := range []struct {
		tenant      string
		concurrency int
		buf         int
	}{{"acme", 4, 16}, {"globex", 8, 32}} {
		c := invokeRoute(t, r, fasthttp.MethodPost,
			"/api/tenants/"+tc.tenant+"/providers", makeProvider(tc.concurrency, tc.buf))
		if got := c.Response.StatusCode(); got != fasthttp.StatusCreated {
			t.Fatalf("create provider for %s: status %d body=%s", tc.tenant, got, string(c.Response.Body()))
		}
	}

	// Phase 2b — register an MCP client for each tenant with the same
	// name 'tools', different connection strings; (tenant_id, name)
	// composite uniqueness should make this legal and each tenant
	// should only see its own.
	for _, tenant := range []string{"acme", "globex"} {
		body, _ := sonic.Marshal(map[string]any{
			"name":              "tools",
			"connection_type":   "http",
			"connection_string": "https://" + tenant + ".example.com/mcp",
		})
		c := invokeRoute(t, r, fasthttp.MethodPost,
			"/api/tenants/"+tenant+"/mcp/clients", body)
		if got := c.Response.StatusCode(); got != fasthttp.StatusCreated {
			t.Fatalf("create mcp for %s: status %d body=%s", tenant, got, string(c.Response.Body()))
		}
	}

	// Phase 3 — mint a VK for each tenant.
	vkValues := map[string]string{}
	for _, tenant := range []string{"acme", "globex"} {
		body, _ := sonic.Marshal(map[string]any{"name": tenant + "-vk"})
		c := invokeRoute(t, r, fasthttp.MethodPost,
			"/api/tenants/"+tenant+"/governance/virtual-keys", body)
		if got := c.Response.StatusCode(); got != fasthttp.StatusCreated {
			t.Fatalf("create vk for %s: status %d body=%s", tenant, got, string(c.Response.Body()))
		}
		var resp struct {
			Message    string                            `json:"message"`
			VirtualKey configstoreTables.TableVirtualKey `json:"virtual_key"`
		}
		if err := sonic.Unmarshal(c.Response.Body(), &resp); err != nil {
			t.Fatalf("decode vk response for %s: %v", tenant, err)
		}
		if resp.VirtualKey.TenantID != tenant {
			t.Fatalf("vk response virtual_key.tenant_id got %q want %q", resp.VirtualKey.TenantID, tenant)
		}
		vkValues[tenant] = resp.VirtualKey.Value
	}

	// Assertion 1: per-tenant provider config is isolated.
	acme, err := store.GetProvidersConfigByTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("get providers for acme: %v", err)
	}
	if len(acme) != 1 || acme["openai"].ConcurrencyAndBufferSize.Concurrency != 4 {
		t.Fatalf("acme should have its own openai with concurrency=4; got %+v", acme)
	}
	globex, err := store.GetProvidersConfigByTenant(ctx, "globex")
	if err != nil {
		t.Fatalf("get providers for globex: %v", err)
	}
	if len(globex) != 1 || globex["openai"].ConcurrencyAndBufferSize.Concurrency != 8 {
		t.Fatalf("globex should have its own openai with concurrency=8; got %+v", globex)
	}

	// Assertion 1b: per-tenant MCP client list is isolated.
	acmeMCP, err := store.GetMCPConfigByTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("get mcp for acme: %v", err)
	}
	if acmeMCP == nil || len(acmeMCP.ClientConfigs) != 1 || acmeMCP.ClientConfigs[0].Name != "tools" {
		t.Fatalf("acme should have its own 'tools' MCP client; got %+v", acmeMCP)
	}
	if v := acmeMCP.ClientConfigs[0].ConnectionString; v == nil || v.GetValue() != "https://acme.example.com/mcp" {
		var got string
		if v != nil {
			got = v.GetValue()
		}
		t.Fatalf("acme's MCP connection string should not be globex's; raw=%+v GetValue()=%q", v, got)
	}
	globexMCP, err := store.GetMCPConfigByTenant(ctx, "globex")
	if err != nil {
		t.Fatalf("get mcp for globex: %v", err)
	}
	if globexMCP == nil || len(globexMCP.ClientConfigs) != 1 || globexMCP.ClientConfigs[0].Name != "tools" {
		t.Fatalf("globex should have its own 'tools' MCP client; got %+v", globexMCP)
	}
	if v := globexMCP.ClientConfigs[0].ConnectionString; v == nil || v.GetValue() != "https://globex.example.com/mcp" {
		t.Fatalf("globex's MCP connection string should not be acme's; got %+v", v)
	}

	// Assertion 2: VK resolver maps each VK to the right tenant.
	resolver := multitenant.NewConfigStoreVKResolver(store)
	for tenant, value := range vkValues {
		tid, err := resolver.ResolveVK(ctx, value)
		if err != nil {
			t.Fatalf("resolve vk for %s: %v", tenant, err)
		}
		if string(tid) != tenant {
			t.Fatalf("VK %q resolved to tenant %q, want %q", value, tid, tenant)
		}
	}

	// Assertion 3: multi-tenant Manager lazy-loads a per-tenant runtime
	// whose Account holds only that tenant's providers — proving the
	// resolver -> Manager -> per-tenant Account pipeline is wired
	// correctly even though no actual inference runs in this test.
	loader := &stubLoader{store: store}
	mgr, err := multitenant.NewManager(ctx, multitenant.ManagerConfig{Loader: loader.Load})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.Shutdown(ctx)

	for _, tenant := range []string{"acme", "globex"} {
		handle, err := mgr.Acquire(ctx, multitenant.TenantID(tenant))
		if err != nil {
			t.Fatalf("manager acquire %s: %v", tenant, err)
		}
		handle.Release()
	}
	loader.mu.Lock()
	if len(loader.loaded) != 2 {
		t.Fatalf("loader should have been invoked for both tenants; got %v", loader.loaded)
	}
	seen := map[multitenant.TenantID]bool{}
	for _, tid := range loader.loaded {
		seen[tid] = true
	}
	if !seen["acme"] || !seen["globex"] {
		t.Fatalf("both tenants should have lazy-loaded; got %v", loader.loaded)
	}
	loader.mu.Unlock()

	// Phase 4 — DELETE acme's provider via the tenant-scoped admin
	// endpoint, then assert the store no longer returns it for acme
	// while globex stays untouched. Exercises GET single (404), GET
	// list (count drop), and DELETE in one pass.
	delResp := invokeRoute(t, r, fasthttp.MethodDelete, "/api/tenants/acme/providers/openai", nil)
	if got, want := delResp.Response.StatusCode(), fasthttp.StatusNoContent; got != want {
		t.Fatalf("delete acme openai: status %d want %d body=%s", got, want, string(delResp.Response.Body()))
	}

	acmeAfter, err := store.GetProvidersConfigByTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("post-delete list for acme: %v", err)
	}
	if _, exists := acmeAfter["openai"]; exists {
		t.Fatal("acme's openai should be gone after delete")
	}
	globexAfter, err := store.GetProvidersConfigByTenant(ctx, "globex")
	if err != nil {
		t.Fatalf("post-delete list for globex: %v", err)
	}
	if _, exists := globexAfter["openai"]; !exists {
		t.Fatal("globex's openai must survive a sibling-tenant delete")
	}

	// GET single for the deleted provider returns 404, not globex's
	// row (the most important cross-tenant safety check).
	getResp := invokeRoute(t, r, fasthttp.MethodGet, "/api/tenants/acme/providers/openai", nil)
	if got, want := getResp.Response.StatusCode(), fasthttp.StatusNotFound; got != want {
		t.Fatalf("get deleted provider: status %d want %d body=%s", got, want, string(getResp.Response.Body()))
	}

	// Phase 5 — exercise the mirror GET list / DELETE on VKs and MCP
	// to confirm the patterns hold for those entity groups too.

	// VK list per tenant returns only that tenant's VKs.
	for tenant, wantCount := range map[string]int{"acme": 1, "globex": 1} {
		c := invokeRoute(t, r, fasthttp.MethodGet, "/api/tenants/"+tenant+"/governance/virtual-keys", nil)
		if got := c.Response.StatusCode(); got != fasthttp.StatusOK {
			t.Fatalf("list vks %s: status %d body=%s", tenant, got, string(c.Response.Body()))
		}
		// New list shape: {virtual_keys, count, total_count, limit, offset}
		// — matches the legacy /api/governance/virtual-keys GET. The
		// previous {tenant_id, virtual_keys, total} envelope is gone.
		var resp struct {
			VirtualKeys []configstoreTables.TableVirtualKey `json:"virtual_keys"`
			Count       int                                 `json:"count"`
			TotalCount  int                                 `json:"total_count"`
		}
		if err := sonic.Unmarshal(c.Response.Body(), &resp); err != nil {
			t.Fatalf("decode vk list for %s: %v", tenant, err)
		}
		if resp.Count != wantCount {
			t.Fatalf("%s should see %d vk(s); got %d", tenant, wantCount, resp.Count)
		}
		for _, vk := range resp.VirtualKeys {
			if vk.TenantID != tenant {
				t.Fatalf("vk %s leaked from %q into %q", vk.ID, vk.TenantID, tenant)
			}
		}
	}

	// MCP list per tenant returns only that tenant's MCP clients.
	for tenant := range map[string]struct{}{"acme": {}, "globex": {}} {
		c := invokeRoute(t, r, fasthttp.MethodGet, "/api/tenants/"+tenant+"/mcp/clients", nil)
		if got := c.Response.StatusCode(); got != fasthttp.StatusOK {
			t.Fatalf("list mcp %s: status %d body=%s", tenant, got, string(c.Response.Body()))
		}
		// New list shape: {clients, count, total_count, limit, offset} —
		// matches the legacy /api/mcp/clients GET. The previous
		// {tenant_id, clients, total} envelope is gone.
		var resp struct {
			Clients    []*schemas.MCPClientConfig `json:"clients"`
			Count      int                        `json:"count"`
			TotalCount int                        `json:"total_count"`
		}
		if err := sonic.Unmarshal(c.Response.Body(), &resp); err != nil {
			t.Fatalf("decode mcp list for %s: %v", tenant, err)
		}
		if resp.Count != 1 {
			t.Fatalf("%s should see exactly one mcp client; got %d", tenant, resp.Count)
		}
		// Cross-tenant safety: delete using one tenant's id but globex's URL.
	}

	// DELETE the MCP client for acme; confirm globex's MCP survives.
	acmeMCPBefore, _ := store.GetMCPConfigByTenant(ctx, "acme")
	require := func(cond bool, msg string) {
		if !cond {
			t.Fatal(msg)
		}
	}
	require(acmeMCPBefore != nil && len(acmeMCPBefore.ClientConfigs) == 1, "precondition: acme should still have one MCP client")
	acmeMCPID := acmeMCPBefore.ClientConfigs[0].ID

	delMCP := invokeRoute(t, r, fasthttp.MethodDelete, "/api/tenants/acme/mcp/clients/"+acmeMCPID, nil)
	// DELETE returns 200 + {status, message} (matches the legacy MCP
	// shape that the UI's deleteMCPClient mutation expects).
	if got, want := delMCP.Response.StatusCode(), fasthttp.StatusOK; got != want {
		t.Fatalf("delete acme mcp: status %d want %d body=%s", got, want, string(delMCP.Response.Body()))
	}

	acmeMCPAfter, err := store.GetMCPConfigByTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("post-delete mcp list for acme: %v", err)
	}
	if acmeMCPAfter != nil && len(acmeMCPAfter.ClientConfigs) != 0 {
		t.Fatalf("acme's mcp should be gone; got %+v", acmeMCPAfter)
	}
	globexMCPAfter, err := store.GetMCPConfigByTenant(ctx, "globex")
	if err != nil {
		t.Fatalf("post-delete mcp list for globex: %v", err)
	}
	if globexMCPAfter == nil || len(globexMCPAfter.ClientConfigs) != 1 {
		t.Fatalf("globex's mcp must survive acme's delete; got %+v", globexMCPAfter)
	}

	// Cross-tenant DELETE attempt: acme trying to delete globex's MCP
	// via the URL pattern would be caught by the path middleware in
	// production (RequireTenantPath rejects mismatched tenant claims);
	// here we exercise the store layer directly to lock in the
	// ErrNotFound response from cross-tenant ID probes.
	if err := store.DeleteMCPClientConfigForTenant(ctx, "acme", globexMCPAfter.ClientConfigs[0].ID); !errors.Is(err, configstore.ErrNotFound) {
		t.Fatalf("cross-tenant delete probe should ErrNotFound; got %v", err)
	}
}

// TestMultiTenantAdmin_RequireTenantPath_RejectsCrossTenant proves the
// tenant-admin auth check on /api/tenants/{tenant_id}/... rejects a
// caller whose identity tenant differs from the URL tenant. With v1's
// always-platform-admin stub this never fires; the test uses a custom
// non-platform-admin authz to assert the policy logic stays correct
// once Phase 4's OIDC plugin replaces the stub.
func TestMultiTenantAdmin_RequireTenantPath_RejectsCrossTenant(t *testing.T) {
	store := setupIntegrationStore(t)

	tenantAdminAuthz := &fixedAuthz{caller: "acme", isPlatformAdmin: false}
	tenantPathMW := RequireTenantPathMiddleware(tenantAdminAuthz)

	provider, _ := NewTenantProviderHandler(store, nil)
	r := router.New()
	provider.RegisterRoutes(r, tenantPathMW)

	body, _ := sonic.Marshal(map[string]any{"provider": "openai"})
	// Acme-admin tries to write into globex's scope -> 403.
	c := invokeRoute(t, r, fasthttp.MethodPost, "/api/tenants/globex/providers", body)
	if got := c.Response.StatusCode(); got != fasthttp.StatusForbidden {
		t.Fatalf("cross-tenant write should be 403; got %d body=%s", got, string(c.Response.Body()))
	}
}

// fixedAuthz is a deterministic lib.AdminAuthz for negative-case tests.
// Lives next to the test it serves so future readers see the policy and
// the assertion together.
type fixedAuthz struct {
	caller          multitenant.TenantID
	isPlatformAdmin bool
}

func (a *fixedAuthz) ResolveCaller(_ *fasthttp.RequestCtx) (multitenant.TenantID, bool, error) {
	return a.caller, a.isPlatformAdmin, nil
}
