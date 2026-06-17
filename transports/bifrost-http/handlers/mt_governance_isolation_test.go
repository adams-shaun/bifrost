// Package handlers — multi-tenant isolation regression tests against the
// header-model governance admin surface.
//
// This file is the mt-header sibling of the URL-path branch's
// mt_apitests_test.go. The URL-path branch reaches a tenant via the
// /api/tenants/{tid}/... URL prefix; mt-header dispatches off the
// x-f5xc-tenant request header, with the configstore's GORM
// scope-by-tenant callback adding `WHERE tenant_id = ?` to every
// SELECT / UPDATE / DELETE under the hood.
//
// What's covered
// --------------
// Two real bugs that the URL-path tests previously caught, ported here
// because both also lived in mt-header until this commit:
//
//  1. Team-name uniqueness MUST be composite (tenant_id, name). The
//     legacy `idx_governance_teams_name` global unique index meant the
//     second tenant trying to create a team called "engineering" got
//     409 with a misleading "already exists for tenant <self>" error.
//     migrationTenantScopedTeamNameUnique fixes the schema; this test
//     asserts both tenants can in fact register the same team name.
//
//  2. Team customer_id MUST belong to the calling tenant. Without the
//     guard, tenant A could create a team whose customer_id points at
//     tenant B's customer — a half-valid row whose FK target the
//     creating tenant cannot read back.
//
// The harness uses a fresh sqlite configstore + a minimal
// GovernanceManager stub so the handler logic runs end-to-end without
// spinning up Bifrost.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/valyala/fasthttp"
)

// ---- harness ---------------------------------------------------------

// stubGovernanceManager satisfies the GovernanceManager interface with
// no-ops; it never gets called for the read paths these tests exercise,
// and the writes only need it to not panic on Reload* callbacks the
// handler triggers after a successful create.
type stubGovernanceManager struct{}

func (stubGovernanceManager) GetGovernanceData(context.Context) *governance.GovernanceData {
	return &governance.GovernanceData{}
}
func (stubGovernanceManager) ReloadVirtualKey(context.Context, string) (*configstoreTables.TableVirtualKey, error) {
	return nil, nil
}
func (stubGovernanceManager) RemoveVirtualKey(context.Context, string) error { return nil }
func (stubGovernanceManager) ReloadTeam(context.Context, string) (*configstoreTables.TableTeam, error) {
	return nil, nil
}
func (stubGovernanceManager) RemoveTeam(context.Context, string) error { return nil }
func (stubGovernanceManager) ReloadCustomer(context.Context, string) (*configstoreTables.TableCustomer, error) {
	return nil, nil
}
func (stubGovernanceManager) RemoveCustomer(context.Context, string) error { return nil }
func (stubGovernanceManager) ReloadModelConfig(context.Context, string) (*configstoreTables.TableModelConfig, error) {
	return nil, nil
}
func (stubGovernanceManager) RemoveModelConfig(context.Context, string) error { return nil }
func (stubGovernanceManager) ReloadProvider(context.Context, schemas.ModelProvider) (*configstoreTables.TableProvider, error) {
	return nil, nil
}
func (stubGovernanceManager) RemoveProvider(context.Context, schemas.ModelProvider) error {
	return nil
}
func (stubGovernanceManager) ReloadRoutingRule(context.Context, string) error  { return nil }
func (stubGovernanceManager) RemoveRoutingRule(context.Context, string) error  { return nil }
func (stubGovernanceManager) UpsertPricingOverride(context.Context, *configstoreTables.TablePricingOverride) error {
	return nil
}
func (stubGovernanceManager) DeletePricingOverride(context.Context, string) error { return nil }

// mtTestEnv assembles a configstore + governance handler + router and
// returns helpers for issuing tenant-scoped admin requests.
type mtIsoEnv struct {
	store  configstore.ConfigStore
	router *router.Router
}

func newMTIsoEnv(t *testing.T) *mtIsoEnv {
	t.Helper()
	SetLogger(&mockLogger{})

	dbPath := t.TempDir() + "/config.db"
	t.Cleanup(func() { _ = os.Remove(dbPath) })
	store, err := configstore.NewConfigStore(context.Background(), &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: dbPath},
	}, &mockLogger{})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}

	govHandler, err := NewGovernanceHandler(stubGovernanceManager{}, store)
	if err != nil {
		t.Fatalf("NewGovernanceHandler: %v", err)
	}

	r := router.New()
	// Mount the same surface server.go mounts under /api/governance/* —
	// no middleware needed because the test injects the tenant id via
	// ctx.SetUserValue below, mirroring what TenantResolverMiddleware
	// would do from a real x-f5xc-tenant header.
	govHandler.RegisterRoutes(r)
	return &mtIsoEnv{store: store, router: r}
}

func (e *mtIsoEnv) do(t *testing.T, tenant, method, path string, body any) *fasthttp.RequestCtx {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	var req fasthttp.Request
	req.SetRequestURI(path)
	req.Header.SetMethod(method)
	if raw != nil {
		req.SetBody(raw)
		req.Header.SetContentType("application/json")
	}
	if tenant != "" {
		req.Header.Set(multitenant.HeaderName, tenant)
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	// Mirror TenantResolverMiddleware's stamp so the configstore's
	// scope-by-tenant GORM callback sees the value through ctx.
	if tenant != "" {
		ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), tenant)
	}
	e.router.Handler(ctx)
	return ctx
}

func mustStatus(t *testing.T, ctx *fasthttp.RequestCtx, want int, hint string) {
	t.Helper()
	if got := ctx.Response.StatusCode(); got != want {
		t.Fatalf("%s: status=%d want=%d body=%s", hint, got, want, string(ctx.Response.Body()))
	}
}

func decode(t *testing.T, ctx *fasthttp.RequestCtx, out any) {
	t.Helper()
	if err := json.Unmarshal(ctx.Response.Body(), out); err != nil {
		t.Fatalf("decode body=%s err=%v", string(ctx.Response.Body()), err)
	}
}

// ---- 1) Team-name composite uniqueness ------------------------------

// TestMTHeader_TeamNameCompositeUnique asserts two tenants can both
// create a team called "engineering" without 409'ing on the second
// create. Before migrationTenantScopedTeamNameUnique landed, this
// failed with "Team engineering already exists for tenant <self>"
// because governance_teams.name had a global single-column UNIQUE.
func TestMTHeader_TeamNameCompositeUnique(t *testing.T) {
	env := newMTIsoEnv(t)

	for _, tid := range []string{"acme", "globex"} {
		body := map[string]any{"name": "engineering"}
		ctx := env.do(t, tid, fasthttp.MethodPost, "/api/governance/teams", body)
		mustStatus(t, ctx, fasthttp.StatusOK, fmt.Sprintf("create team in %s", tid))
	}

	// Both rows should exist with their respective tenant_ids.
	var teams []configstoreTables.TableTeam
	if err := env.store.DB().Find(&teams, "name = ?", "engineering").Error; err != nil {
		t.Fatalf("query teams: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("expected 2 'engineering' teams (one per tenant); got %d", len(teams))
	}
	seen := map[string]bool{}
	for _, tm := range teams {
		seen[tm.TenantID] = true
	}
	for _, tid := range []string{"acme", "globex"} {
		if !seen[tid] {
			t.Fatalf("expected an 'engineering' row stamped with tenant_id=%q; got %+v", tid, teams)
		}
	}
}

// TestMTHeader_TeamNameDuplicateWithinTenant confirms uniqueness still
// holds in-tenant — the second create with the same name in the same
// tenant gets an error response rather than silently succeeding. The
// mt-header createTeam handler currently surfaces this as a 500 with a
// generic "failed to create team" body (the configstore's unique-index
// violation isn't translated to ErrAlreadyExists → 409 there); we
// accept any 4xx/5xx until that handler gets its own polish pass. The
// thing that matters here is that the second row does NOT land.
func TestMTHeader_TeamNameDuplicateWithinTenant(t *testing.T) {
	env := newMTIsoEnv(t)

	first := env.do(t, "acme", fasthttp.MethodPost, "/api/governance/teams",
		map[string]any{"name": "engineering"})
	mustStatus(t, first, fasthttp.StatusOK, "first create")

	second := env.do(t, "acme", fasthttp.MethodPost, "/api/governance/teams",
		map[string]any{"name": "engineering"})
	if got := second.Response.StatusCode(); got < 400 {
		t.Fatalf("duplicate-in-tenant create: status=%d want >=400 body=%s",
			got, string(second.Response.Body()))
	}

	// Confirm only one row landed.
	var teams []configstoreTables.TableTeam
	if err := env.store.DB().Find(&teams, "tenant_id = ? AND name = ?", "acme", "engineering").Error; err != nil {
		t.Fatalf("query teams: %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("after duplicate create, expected 1 'engineering' team in acme; got %d", len(teams))
	}
}

// ---- 2) Team customer_id cross-tenant FK guard ----------------------

// firstCustomerID returns the id of the (one and only) customer this
// test seeded for a tenant by querying the store directly. The HTTP
// create endpoints return {"customer":null, "message":"..."} so the
// row id isn't in the response.
func firstCustomerID(t *testing.T, env *mtIsoEnv, tenant string) string {
	t.Helper()
	var customers []configstoreTables.TableCustomer
	if err := env.store.DB().Find(&customers, "tenant_id = ?", tenant).Error; err != nil {
		t.Fatalf("query customers for %s: %v", tenant, err)
	}
	if len(customers) == 0 {
		t.Fatalf("no customers seeded for %s", tenant)
	}
	return customers[0].ID
}

func firstTeamID(t *testing.T, env *mtIsoEnv, tenant string) string {
	t.Helper()
	var teams []configstoreTables.TableTeam
	if err := env.store.DB().Find(&teams, "tenant_id = ?", tenant).Error; err != nil {
		t.Fatalf("query teams for %s: %v", tenant, err)
	}
	if len(teams) == 0 {
		t.Fatalf("no teams seeded for %s", tenant)
	}
	return teams[0].ID
}

// TestMTHeader_TeamCrossTenantCustomerFKRejected asserts that creating
// a team whose customer_id belongs to a different tenant returns 400.
// The handler now calls configStore.GetCustomer(ctx, id) which is
// tenant-scoped, so a foreign customer returns ErrNotFound → 400.
func TestMTHeader_TeamCrossTenantCustomerFKRejected(t *testing.T) {
	env := newMTIsoEnv(t)

	// Seed globex with a customer.
	custCtx := env.do(t, "globex", fasthttp.MethodPost, "/api/governance/customers",
		map[string]any{"name": "globex-eng"})
	mustStatus(t, custCtx, fasthttp.StatusOK, "create globex customer")
	globexCustID := firstCustomerID(t, env, "globex")

	// Acme attempts to attach globex's customer to a new team.
	teamCtx := env.do(t, "acme", fasthttp.MethodPost, "/api/governance/teams", map[string]any{
		"name":        "leaky",
		"customer_id": globexCustID,
	})
	if got := teamCtx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("cross-tenant customer_id should 400; got %d body=%s",
			got, string(teamCtx.Response.Body()))
	}
	if !bytes.Contains(teamCtx.Response.Body(), []byte("customer_id")) {
		t.Fatalf("expected error to mention customer_id; body=%s",
			string(teamCtx.Response.Body()))
	}

	// And confirm no acme team row landed (the guard runs before insert).
	var acmeTeams []configstoreTables.TableTeam
	if err := env.store.DB().Find(&acmeTeams, "tenant_id = ?", "acme").Error; err != nil {
		t.Fatalf("query acme teams: %v", err)
	}
	if len(acmeTeams) != 0 {
		t.Fatalf("rejected cross-tenant create left an acme team behind: %+v", acmeTeams)
	}
}

// TestMTHeader_TeamUpdateCrossTenantCustomerFKRejected mirrors the
// create test for the PUT path. Same code shape, same guard.
func TestMTHeader_TeamUpdateCrossTenantCustomerFKRejected(t *testing.T) {
	env := newMTIsoEnv(t)

	// Seed: acme has a team with no customer; globex has a customer.
	teamCtx := env.do(t, "acme", fasthttp.MethodPost, "/api/governance/teams",
		map[string]any{"name": "platform"})
	mustStatus(t, teamCtx, fasthttp.StatusOK, "create acme team")
	acmeTeamID := firstTeamID(t, env, "acme")

	custCtx := env.do(t, "globex", fasthttp.MethodPost, "/api/governance/customers",
		map[string]any{"name": "globex-cust"})
	mustStatus(t, custCtx, fasthttp.StatusOK, "create globex customer")
	globexCustID := firstCustomerID(t, env, "globex")

	// Acme tries to PUT the team with globex's customer_id.
	putCtx := env.do(t, "acme", fasthttp.MethodPut,
		"/api/governance/teams/"+acmeTeamID,
		map[string]any{"customer_id": globexCustID})
	if got := putCtx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("cross-tenant customer_id on PUT should 400; got %d body=%s",
			got, string(putCtx.Response.Body()))
	}
	if !bytes.Contains(putCtx.Response.Body(), []byte("customer_id")) {
		t.Fatalf("expected PUT error to mention customer_id; body=%s",
			string(putCtx.Response.Body()))
	}
}

// ---- silence unused-import lints on the trim path -------------------

var _ = errors.Is
