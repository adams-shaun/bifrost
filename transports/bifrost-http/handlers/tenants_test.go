package handlers

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// newRequestCtx mirrors newTestRequestCtx in pricing_override_test.go — a
// bare &fasthttp.RequestCtx{} panics on Done() because its internal Server
// reference is nil, and gorm.Begin reads Done() before issuing the
// transaction. ctx.Init wires a valid remote-addr so Done() works.
func newRequestCtx(method, body string) *fasthttp.RequestCtx {
	var req fasthttp.Request
	req.Header.SetMethod(method)
	if body != "" {
		req.SetBodyString(body)
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	return ctx
}

// newTenantsTestHandler spins up a fresh SQLite-backed ConfigStore + handler
// for a single test. The migration chain seeds the default tenant.
func newTenantsTestHandler(t *testing.T) (*TenantsHandler, configstore.ConfigStore) {
	t.Helper()
	SetLogger(&mockLogger{})

	dbPath := filepath.Join(t.TempDir(), "tenants.db")
	store, err := configstore.NewConfigStore(context.Background(), &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: dbPath},
	}, &mockLogger{})
	require.NoError(t, err)

	h, err := NewTenantsHandler(store)
	require.NoError(t, err)
	return h, store
}

func decodeTenantsBody[T any](t *testing.T, ctx *fasthttp.RequestCtx) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &out))
	return out
}

func TestTenantsHandler_Create_Success(t *testing.T) {
	h, _ := newTenantsTestHandler(t)

	ctx := newRequestCtx(fasthttp.MethodPost, `{"id":"acme","name":"Acme Corp"}`)
	h.createTenant(ctx)

	require.Equal(t, fasthttp.StatusCreated, ctx.Response.StatusCode(),
		"body: %s", string(ctx.Response.Body()))
	body := decodeTenantsBody[struct {
		Message string                       `json:"message"`
		Tenant  configstoreTables.TableTenant `json:"tenant"`
	}](t, ctx)
	assert.Equal(t, "acme", body.Tenant.ID)
	assert.Equal(t, "Acme Corp", body.Tenant.Name)
	assert.Equal(t, configstoreTables.TenantStatusActive, body.Tenant.Status,
		"status should default to active when omitted")
}

func TestTenantsHandler_Create_MissingFields(t *testing.T) {
	h, _ := newTenantsTestHandler(t)

	cases := []struct {
		name string
		body string
	}{
		{"missing id", `{"name":"x"}`},
		{"missing name", `{"id":"x"}`},
		{"invalid status", `{"id":"x","name":"x","status":"frozen"}`},
		{"malformed json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newRequestCtx(fasthttp.MethodPost, tc.body)
			h.createTenant(ctx)
			assert.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode())
		})
	}
}

func TestTenantsHandler_Create_Duplicate(t *testing.T) {
	h, _ := newTenantsTestHandler(t)

	ctx1 := newRequestCtx(fasthttp.MethodPost, `{"id":"acme","name":"Acme"}`)
	h.createTenant(ctx1)
	require.Equal(t, fasthttp.StatusCreated, ctx1.Response.StatusCode())

	ctx2 := newRequestCtx(fasthttp.MethodPost, `{"id":"acme","name":"Acme Again"}`)
	h.createTenant(ctx2)
	assert.Equal(t, fasthttp.StatusConflict, ctx2.Response.StatusCode())
}

func TestTenantsHandler_Get(t *testing.T) {
	h, store := newTenantsTestHandler(t)

	require.NoError(t, store.CreateTenant(context.Background(), &configstoreTables.TableTenant{
		ID:     "acme",
		Name:   "Acme",
		Status: configstoreTables.TenantStatusActive,
	}))

	ctx := newRequestCtx(fasthttp.MethodGet, "")
	ctx.SetUserValue("tenant_id", "acme")
	h.getTenant(ctx)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	body := decodeTenantsBody[struct {
		Tenant configstoreTables.TableTenant `json:"tenant"`
	}](t, ctx)
	assert.Equal(t, "acme", body.Tenant.ID)
}

func TestTenantsHandler_Get_NotFound(t *testing.T) {
	h, _ := newTenantsTestHandler(t)
	ctx := newRequestCtx(fasthttp.MethodGet, "")
	ctx.SetUserValue("tenant_id", "nope")
	h.getTenant(ctx)
	assert.Equal(t, fasthttp.StatusNotFound, ctx.Response.StatusCode())
}

func TestTenantsHandler_List_PagingAndFilters(t *testing.T) {
	h, store := newTenantsTestHandler(t)
	bg := context.Background()

	for _, id := range []string{"alpha", "beta", "gamma"} {
		require.NoError(t, store.CreateTenant(bg, &configstoreTables.TableTenant{
			ID: id, Name: id, Status: configstoreTables.TenantStatusActive,
		}))
	}
	// Flip beta to suspended.
	beta, err := store.GetTenant(bg, "beta")
	require.NoError(t, err)
	beta.Status = configstoreTables.TenantStatusSuspended
	require.NoError(t, store.UpdateTenant(bg, beta))

	// All — default + 3 we created.
	ctx := newRequestCtx(fasthttp.MethodGet, "")
	h.listTenants(ctx)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	type listResp struct {
		Tenants    []configstoreTables.TableTenant `json:"tenants"`
		Count      int                             `json:"count"`
		TotalCount int                             `json:"total_count"`
	}
	body := decodeTenantsBody[listResp](t, ctx)
	assert.GreaterOrEqual(t, body.TotalCount, 4)

	// Status filter.
	ctx2 := newRequestCtx(fasthttp.MethodGet, "")
	ctx2.QueryArgs().Add("status", configstoreTables.TenantStatusSuspended)
	h.listTenants(ctx2)
	require.Equal(t, fasthttp.StatusOK, ctx2.Response.StatusCode())
	body2 := decodeTenantsBody[listResp](t, ctx2)
	require.Equal(t, 1, body2.Count)
	assert.Equal(t, "beta", body2.Tenants[0].ID)

	// Search.
	ctx3 := newRequestCtx(fasthttp.MethodGet, "")
	ctx3.QueryArgs().Add("search", "alph")
	h.listTenants(ctx3)
	require.Equal(t, fasthttp.StatusOK, ctx3.Response.StatusCode())
	body3 := decodeTenantsBody[listResp](t, ctx3)
	require.Equal(t, 1, body3.Count)
	assert.Equal(t, "alpha", body3.Tenants[0].ID)
}

func TestTenantsHandler_Update(t *testing.T) {
	h, store := newTenantsTestHandler(t)
	bg := context.Background()
	require.NoError(t, store.CreateTenant(bg, &configstoreTables.TableTenant{
		ID: "acme", Name: "Acme", Status: configstoreTables.TenantStatusActive,
	}))

	ctx := newRequestCtx(fasthttp.MethodPut, `{"name":"Acme (renamed)","status":"suspended","description":"on hold"}`)
	ctx.SetUserValue("tenant_id", "acme")
	h.updateTenant(ctx)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(),
		"body: %s", string(ctx.Response.Body()))

	got, err := store.GetTenant(bg, "acme")
	require.NoError(t, err)
	assert.Equal(t, "Acme (renamed)", got.Name)
	assert.Equal(t, configstoreTables.TenantStatusSuspended, got.Status)
	assert.Equal(t, "on hold", got.Description)
}

func TestTenantsHandler_Update_NotFound(t *testing.T) {
	h, _ := newTenantsTestHandler(t)
	ctx := newRequestCtx(fasthttp.MethodPut, `{"name":"x"}`)
	ctx.SetUserValue("tenant_id", "nope")
	h.updateTenant(ctx)
	assert.Equal(t, fasthttp.StatusNotFound, ctx.Response.StatusCode())
}

func TestTenantsHandler_Update_InvalidStatus(t *testing.T) {
	h, store := newTenantsTestHandler(t)
	require.NoError(t, store.CreateTenant(context.Background(), &configstoreTables.TableTenant{
		ID: "acme", Name: "Acme", Status: configstoreTables.TenantStatusActive,
	}))

	ctx := newRequestCtx(fasthttp.MethodPut, `{"status":"frozen"}`)
	ctx.SetUserValue("tenant_id", "acme")
	h.updateTenant(ctx)
	assert.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode())
}

func TestTenantsHandler_Delete_Empty(t *testing.T) {
	h, store := newTenantsTestHandler(t)
	require.NoError(t, store.CreateTenant(context.Background(), &configstoreTables.TableTenant{
		ID: "acme", Name: "Acme", Status: configstoreTables.TenantStatusActive,
	}))

	ctx := newRequestCtx(fasthttp.MethodDelete, "")
	ctx.SetUserValue("tenant_id", "acme")
	h.deleteTenant(ctx)
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())

	_, err := store.GetTenant(context.Background(), "acme")
	assert.ErrorIs(t, err, configstore.ErrNotFound)
}

func TestTenantsHandler_Delete_Default_Refused(t *testing.T) {
	h, _ := newTenantsTestHandler(t)
	ctx := newRequestCtx(fasthttp.MethodDelete, "")
	ctx.SetUserValue("tenant_id", configstoreTables.DefaultTenantID)
	h.deleteTenant(ctx)
	assert.Equal(t, fasthttp.StatusConflict, ctx.Response.StatusCode())
}

func TestTenantsHandler_Delete_NotEmpty_Refused(t *testing.T) {
	h, store := newTenantsTestHandler(t)
	bg := context.Background()
	require.NoError(t, store.CreateTenant(bg, &configstoreTables.TableTenant{
		ID: "owns-stuff", Name: "Owns Stuff", Status: configstoreTables.TenantStatusActive,
	}))
	require.NoError(t, store.CreateCustomer(bg, &configstoreTables.TableCustomer{
		ID: "cust-1", Name: "Cust", TenantID: "owns-stuff",
	}))

	ctx := newRequestCtx(fasthttp.MethodDelete, "")
	ctx.SetUserValue("tenant_id", "owns-stuff")
	h.deleteTenant(ctx)
	assert.Equal(t, fasthttp.StatusConflict, ctx.Response.StatusCode())

	// Cleaning the customer up unblocks delete.
	require.NoError(t, store.DeleteCustomer(bg, "cust-1"))
	ctx2 := newRequestCtx(fasthttp.MethodDelete, "")
	ctx2.SetUserValue("tenant_id", "owns-stuff")
	h.deleteTenant(ctx2)
	assert.Equal(t, fasthttp.StatusOK, ctx2.Response.StatusCode())
}

func TestTenantsHandler_Delete_NotFound(t *testing.T) {
	h, _ := newTenantsTestHandler(t)
	ctx := newRequestCtx(fasthttp.MethodDelete, "")
	ctx.SetUserValue("tenant_id", "nope")
	h.deleteTenant(ctx)
	assert.Equal(t, fasthttp.StatusNotFound, ctx.Response.StatusCode())
}
