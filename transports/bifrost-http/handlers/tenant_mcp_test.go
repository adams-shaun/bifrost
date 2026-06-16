package handlers

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

type stubMCPStore struct {
	configstore.ConfigStore // nil embedded; unimplemented methods panic if hit
	mu                      sync.Mutex
	created                 []mcpCreateForTenantCall
	stored                  []*configstoreTables.TableMCPClient
	createErr               error
}

type mcpCreateForTenantCall struct {
	tenantID string
	config   *schemas.MCPClientConfig
}

func (s *stubMCPStore) CreateMCPClientConfigForTenant(ctx context.Context, tenantID string, cfg *schemas.MCPClientConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	cp := *cfg
	s.created = append(s.created, mcpCreateForTenantCall{tenantID: tenantID, config: &cp})
	return nil
}

func newTenantMCPCtx(t *testing.T, body string, tid string) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/" + tid + "/mcp/clients")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBodyString(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	if tid != "" {
		ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), tid)
	}
	return ctx
}

func TestTenantMCPHandler_Create_HappyPath_MintsClientID(t *testing.T) {
	store := &stubMCPStore{}
	evictor := &stubEvictor{}
	h, err := NewTenantMCPHandler(store, evictor)
	if err != nil {
		t.Fatalf("NewTenantMCPHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{
		"name":              "tools",
		"connection_type":   "http",
		"connection_string": "https://acme.example.com/mcp",
	})
	ctx := newTenantMCPCtx(t, string(body), "acme")

	h.createMCPClient(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.created) != 1 {
		t.Fatalf("CreateMCPClientConfigForTenant calls got %d want 1", len(store.created))
	}
	call := store.created[0]
	if call.tenantID != "acme" {
		t.Fatalf("tenant id got %q want acme", call.tenantID)
	}
	if call.config.Name != "tools" {
		t.Fatalf("name got %q want tools", call.config.Name)
	}
	if call.config.ID == "" {
		t.Fatal("server should have minted a client id when omitted")
	}

	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 || evictor.called[0] != multitenant.TenantID("acme") {
		t.Fatalf("Evict should fire for acme; got %v", evictor.called)
	}
}

func TestTenantMCPHandler_Create_RespectsExplicitID(t *testing.T) {
	store := &stubMCPStore{}
	h, _ := NewTenantMCPHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{
		"id":              "mcp-fixed-id",
		"name":            "tools",
		"connection_type": "http",
	})
	ctx := newTenantMCPCtx(t, string(body), "acme")

	h.createMCPClient(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.created[0].config.ID != "mcp-fixed-id" {
		t.Fatalf("explicit id should pass through; got %q", store.created[0].config.ID)
	}
}

func TestTenantMCPHandler_Create_MissingName(t *testing.T) {
	store := &stubMCPStore{}
	h, _ := NewTenantMCPHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{"connection_type": "http"})
	ctx := newTenantMCPCtx(t, string(body), "acme")

	h.createMCPClient(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("missing name should be 400; got %d", got)
	}
	if len(store.created) != 0 {
		t.Fatal("store must not be called when payload invalid")
	}
}

func TestTenantMCPHandler_Create_MissingConnectionType(t *testing.T) {
	store := &stubMCPStore{}
	h, _ := NewTenantMCPHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "tools"})
	ctx := newTenantMCPCtx(t, string(body), "acme")

	h.createMCPClient(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("missing connection_type should be 400; got %d", got)
	}
}

func TestTenantMCPHandler_Create_MissingTenantCtx(t *testing.T) {
	store := &stubMCPStore{}
	h, _ := NewTenantMCPHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "tools", "connection_type": "http"})
	ctx := newTenantMCPCtx(t, string(body), "")

	h.createMCPClient(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("missing tenant ctx should be 500; got %d", got)
	}
}

func TestTenantMCPHandler_NewWithNilStore(t *testing.T) {
	if _, err := NewTenantMCPHandler(nil, nil); err == nil {
		t.Fatal("nil store should error")
	}
}

// stubMCPStore extensions used by GET single + PUT tests.

func (s *stubMCPStore) GetMCPClientByIDForTenant(_ context.Context, tenantID, id string) (*configstoreTables.TableMCPClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.stored {
		if c.ClientID == id && c.TenantID == tenantID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubMCPStore) UpdateMCPClientConfigForTenant(_ context.Context, tenantID, id string, cfg *configstoreTables.TableMCPClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.stored {
		if existing.ClientID == id && existing.TenantID == tenantID {
			cp := *cfg
			cp.TenantID = tenantID
			s.stored[i] = &cp
			return nil
		}
	}
	return configstore.ErrNotFound
}

func TestTenantMCPHandler_Update_HappyPath_Evicts(t *testing.T) {
	store := &stubMCPStore{}
	store.stored = append(store.stored, &configstoreTables.TableMCPClient{
		ClientID: "mcp-1", TenantID: "acme", Name: "tools", ConnectionType: "http", Disabled: false,
	})
	evictor := &stubEvictor{}
	h, _ := NewTenantMCPHandler(store, evictor)

	body, _ := sonic.Marshal(map[string]any{"disabled": true})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/mcp/clients/mcp-1")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("client_id", "mcp-1")

	h.updateMCPClient(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusOK; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.stored[0].Disabled {
		t.Fatal("disabled flag should have flipped")
	}
	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 || evictor.called[0] != multitenant.TenantID("acme") {
		t.Fatalf("Evict should fire; got %v", evictor.called)
	}
}

func TestTenantMCPHandler_Update_CrossTenantReturns404(t *testing.T) {
	store := &stubMCPStore{}
	store.stored = append(store.stored, &configstoreTables.TableMCPClient{
		ClientID: "mcp-secret", TenantID: "globex", Name: "tools", ConnectionType: "http",
	})
	h, _ := NewTenantMCPHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{"disabled": true})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/mcp/clients/mcp-secret")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("client_id", "mcp-secret")

	h.updateMCPClient(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant put should be 404; got %d", got)
	}
	// globex's row must not have flipped.
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.stored[0].Disabled {
		t.Fatal("globex's row should not have been mutated by acme's PUT attempt")
	}
}

func TestTenantMCPHandler_Update_RejectsEmptyBody(t *testing.T) {
	store := &stubMCPStore{}
	store.stored = append(store.stored, &configstoreTables.TableMCPClient{ClientID: "mcp-1", TenantID: "acme"})
	h, _ := NewTenantMCPHandler(store, nil)

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/mcp/clients/mcp-1")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody([]byte(`{}`))
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("client_id", "mcp-1")

	h.updateMCPClient(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("empty patch body should be 400; got %d", got)
	}
}
