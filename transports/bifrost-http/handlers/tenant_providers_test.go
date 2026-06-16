package handlers

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// stubProviderStore is the minimum ConfigStore surface this handler needs.
// All other methods panic; tests should never hit them.
type stubProviderStore struct {
	configstore.ConfigStore // embeds nil → unimplemented methods panic
	mu                      sync.Mutex
	calls                   []addProviderForTenantCall
	addErr                  error
}

type addProviderForTenantCall struct {
	tenantID string
	provider schemas.ModelProvider
	config   configstore.ProviderConfig
}

func (s *stubProviderStore) AddProviderForTenant(ctx context.Context, tenantID string, provider schemas.ModelProvider, config configstore.ProviderConfig, tx ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, addProviderForTenantCall{tenantID: tenantID, provider: provider, config: config})
	return s.addErr
}

type stubEvictor struct {
	mu     sync.Mutex
	called []multitenant.TenantID
}

func (e *stubEvictor) Evict(tid multitenant.TenantID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.called = append(e.called, tid)
	return true
}

func newTenantProviderCtx(t *testing.T, body string, tid string) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/" + tid + "/providers")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBodyString(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	if tid != "" {
		ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), tid)
	}
	return ctx
}

func TestTenantProviderHandler_CreateProvider_HappyPath(t *testing.T) {
	store := &stubProviderStore{}
	evictor := &stubEvictor{}
	h, err := NewTenantProviderHandler(store, evictor)
	if err != nil {
		t.Fatalf("NewTenantProviderHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{
		"provider": "openai",
		"concurrency_and_buffer_size": map[string]any{
			"concurrency": 4,
			"buffer_size": 16,
		},
	})
	ctx := newTenantProviderCtx(t, string(body), "acme")

	h.createProvider(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.calls); got != 1 {
		t.Fatalf("AddProviderForTenant calls got %d want 1", got)
	}
	if got, want := store.calls[0].tenantID, "acme"; got != want {
		t.Fatalf("tenant id got %q want %q", got, want)
	}
	if got, want := string(store.calls[0].provider), "openai"; got != want {
		t.Fatalf("provider got %q want %q", got, want)
	}
	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if got := len(evictor.called); got != 1 {
		t.Fatalf("Evict calls got %d want 1", got)
	}
	if got, want := evictor.called[0], multitenant.TenantID("acme"); got != want {
		t.Fatalf("evicted tid got %q want %q", got, want)
	}
}

func TestTenantProviderHandler_CreateProvider_MissingTenantCtx(t *testing.T) {
	store := &stubProviderStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{"provider": "openai"})
	ctx := newTenantProviderCtx(t, string(body), "") // no tenant_id on ctx

	h.createProvider(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("status got %d want 500", got)
	}
	if len(store.calls) != 0 {
		t.Fatal("store must not be called when tenant_id missing")
	}
}

func TestTenantProviderHandler_CreateProvider_MissingProvider(t *testing.T) {
	store := &stubProviderStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	ctx := newTenantProviderCtx(t, `{}`, "acme")

	h.createProvider(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("status got %d want 400", got)
	}
	if len(store.calls) != 0 {
		t.Fatal("store must not be called when payload invalid")
	}
}

func TestTenantProviderHandler_CreateProvider_NilEvictorIsAllowed(t *testing.T) {
	store := &stubProviderStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{
		"provider": "openai",
	})
	ctx := newTenantProviderCtx(t, string(body), "acme")

	h.createProvider(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	if len(store.calls) != 1 {
		t.Fatalf("store should still receive the write; got %d calls", len(store.calls))
	}
}

func TestTenantProviderHandler_NewWithNilStore(t *testing.T) {
	if _, err := NewTenantProviderHandler(nil, nil); err == nil {
		t.Fatal("nil store should error")
	}
}

func TestTenantProviderHandler_CreateProvider_StoreError(t *testing.T) {
	store := &stubProviderStore{addErr: configstore.ErrAlreadyExists}
	h, _ := NewTenantProviderHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{"provider": "openai"})
	ctx := newTenantProviderCtx(t, string(body), "acme")

	h.createProvider(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusConflict {
		t.Fatalf("ErrAlreadyExists should map to 409, got %d", got)
	}
}

// Extend stubProviderStore with GET + DELETE surfaces so the read/write
// CRUD handlers can be exercised without dragging in a real DB.

func (s *stubProviderStore) GetProvidersConfigByTenant(_ context.Context, tenantID string) (map[schemas.ModelProvider]configstore.ProviderConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[schemas.ModelProvider]configstore.ProviderConfig{}
	for _, call := range s.calls {
		if call.tenantID == tenantID {
			out[call.provider] = call.config
		}
	}
	return out, nil
}

func (s *stubProviderStore) GetProviderConfigByTenant(_ context.Context, tenantID string, provider schemas.ModelProvider) (*configstore.ProviderConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.calls {
		if call.tenantID == tenantID && call.provider == provider {
			cfg := call.config
			return &cfg, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubProviderStore) DeleteProviderForTenant(_ context.Context, tenantID string, provider schemas.ModelProvider, tx ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, call := range s.calls {
		if call.tenantID == tenantID && call.provider == provider {
			s.calls = append(s.calls[:i], s.calls[i+1:]...)
			return nil
		}
	}
	return configstore.ErrNotFound
}

func TestTenantProviderHandler_ListProviders_FiltersByTenant(t *testing.T) {
	store := &stubProviderStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	// Seed: two providers for acme, one for globex.
	for _, c := range []addProviderForTenantCall{
		{tenantID: "acme", provider: "openai", config: configstore.ProviderConfig{}},
		{tenantID: "acme", provider: "anthropic", config: configstore.ProviderConfig{}},
		{tenantID: "globex", provider: "openai", config: configstore.ProviderConfig{}},
	} {
		store.calls = append(store.calls, c)
	}

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/providers")
	req.Header.SetMethod(fasthttp.MethodGet)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")

	h.listProviders(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusOK; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	// Response shape mirrors the legacy ListProvidersResponse: an
	// ARRAY of provider rows (each with name/network_config/...), plus
	// total. Map-keyed-by-name would force the UI's getProviders cache
	// into a list of empty ModelProvider objects, breaking sort + sidebar.
	var resp struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
		Total int `json:"total"`
	}
	if err := sonic.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("acme should see 2 providers; got %d (providers=%v)", resp.Total, resp.Providers)
	}
	names := make(map[string]bool, len(resp.Providers))
	for _, p := range resp.Providers {
		names[p.Name] = true
	}
	if !names["openai"] {
		t.Fatal("acme should see openai")
	}
	if !names["anthropic"] {
		t.Fatal("acme should see anthropic")
	}
}

func TestTenantProviderHandler_GetProvider_NotFound(t *testing.T) {
	store := &stubProviderStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	// Provision for globex; acme should not find openai.
	store.calls = append(store.calls, addProviderForTenantCall{tenantID: "globex", provider: "openai"})

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/providers/openai")
	req.Header.SetMethod(fasthttp.MethodGet)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("provider", "openai")

	h.getProvider(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusNotFound; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
}

func TestTenantProviderHandler_DeleteProvider_HappyPath_Evicts(t *testing.T) {
	store := &stubProviderStore{}
	evictor := &stubEvictor{}
	h, _ := NewTenantProviderHandler(store, evictor)

	store.calls = append(store.calls, addProviderForTenantCall{tenantID: "acme", provider: "openai"})

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/providers/openai")
	req.Header.SetMethod(fasthttp.MethodDelete)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("provider", "openai")

	h.deleteProvider(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusNoContent; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	if len(store.calls) != 0 {
		t.Fatalf("store should no longer have the provider; calls=%v", store.calls)
	}
	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 || evictor.called[0] != multitenant.TenantID("acme") {
		t.Fatalf("Evict should fire for acme; got %v", evictor.called)
	}
}

func TestTenantProviderHandler_DeleteProvider_NotFound(t *testing.T) {
	store := &stubProviderStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/providers/openai")
	req.Header.SetMethod(fasthttp.MethodDelete)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("provider", "openai")

	h.deleteProvider(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusNotFound; got != want {
		t.Fatalf("status got %d want %d", got, want)
	}
}

// ---- Provider keys: stub extensions + tests ----

type providerKeyCall struct {
	tenantID string
	provider schemas.ModelProvider
	key      schemas.Key
}

// stubKeyStore embeds stubProviderStore so it inherits provider stubs
// and adds tenant-scoped key bookkeeping. Kept separate from
// stubProviderStore to avoid bloating the simpler provider tests.
type stubKeyStore struct {
	stubProviderStore
	keyCalls []providerKeyCall
	createErr error
}

func (s *stubKeyStore) CreateProviderKeyForTenant(_ context.Context, tenantID string, provider schemas.ModelProvider, key schemas.Key, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	// Require the parent provider to exist for the calling tenant.
	parentExists := false
	for _, c := range s.calls {
		if c.tenantID == tenantID && c.provider == provider {
			parentExists = true
			break
		}
	}
	if !parentExists {
		return configstore.ErrNotFound
	}
	s.keyCalls = append(s.keyCalls, providerKeyCall{tenantID: tenantID, provider: provider, key: key})
	return nil
}

func (s *stubKeyStore) GetProviderKeysForTenant(_ context.Context, tenantID string, provider schemas.ModelProvider) ([]schemas.Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parentExists := false
	for _, c := range s.calls {
		if c.tenantID == tenantID && c.provider == provider {
			parentExists = true
			break
		}
	}
	if !parentExists {
		return nil, configstore.ErrNotFound
	}
	out := []schemas.Key{}
	for _, k := range s.keyCalls {
		if k.tenantID == tenantID && k.provider == provider {
			out = append(out, k.key)
		}
	}
	return out, nil
}

func (s *stubKeyStore) GetProviderKeyForTenant(_ context.Context, tenantID string, provider schemas.ModelProvider, keyID string) (*schemas.Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keyCalls {
		if k.tenantID == tenantID && k.provider == provider && k.key.ID == keyID {
			cp := k.key
			return &cp, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubKeyStore) DeleteProviderKeyForTenant(_ context.Context, tenantID string, provider schemas.ModelProvider, keyID string, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keyCalls {
		if k.tenantID == tenantID && k.provider == provider && k.key.ID == keyID {
			s.keyCalls = append(s.keyCalls[:i], s.keyCalls[i+1:]...)
			return nil
		}
	}
	return configstore.ErrNotFound
}

func newKeyCtx(t *testing.T, method, path, body, tid, provider, keyID string) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	req.SetRequestURI(path)
	req.Header.SetMethod(method)
	if body != "" {
		req.SetBodyString(body)
		req.Header.SetContentType("application/json")
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	if tid != "" {
		ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), tid)
	}
	if provider != "" {
		ctx.SetUserValue("provider", provider)
	}
	if keyID != "" {
		ctx.SetUserValue("key_id", keyID)
	}
	return ctx
}

func TestTenantProviderHandler_CreateProviderKey_HappyPath_Evicts(t *testing.T) {
	store := &stubKeyStore{}
	evictor := &stubEvictor{}
	store.calls = append(store.calls, addProviderForTenantCall{tenantID: "acme", provider: "openai"})

	h, _ := NewTenantProviderHandler(store, evictor)

	body, _ := sonic.Marshal(map[string]any{"name": "primary", "value": "sk-acme-1"})
	ctx := newKeyCtx(t, fasthttp.MethodPost, "/api/tenants/acme/providers/openai/keys", string(body), "acme", "openai", "")

	h.createProviderKey(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	if len(store.keyCalls) != 1 {
		t.Fatalf("expected 1 key created; got %d", len(store.keyCalls))
	}
	if store.keyCalls[0].key.ID == "" {
		t.Fatal("server should mint a key id when omitted")
	}
	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 || evictor.called[0] != multitenant.TenantID("acme") {
		t.Fatalf("Evict should fire for acme; got %v", evictor.called)
	}
}

func TestTenantProviderHandler_CreateProviderKey_ParentNotFound(t *testing.T) {
	store := &stubKeyStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "primary", "value": "sk-x"})
	ctx := newKeyCtx(t, fasthttp.MethodPost, "/api/tenants/acme/providers/openai/keys", string(body), "acme", "openai", "")

	h.createProviderKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("missing parent should be 404; got %d body=%s", got, string(ctx.Response.Body()))
	}
}

func TestTenantProviderHandler_CreateProviderKey_MissingFields(t *testing.T) {
	store := &stubKeyStore{}
	h, _ := NewTenantProviderHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{}) // both name + value missing
	ctx := newKeyCtx(t, fasthttp.MethodPost, "/api/tenants/acme/providers/openai/keys", string(body), "acme", "openai", "")

	h.createProviderKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("missing name+value should be 400; got %d", got)
	}
}

func TestTenantProviderHandler_ListProviderKeys_RedactsValues(t *testing.T) {
	store := &stubKeyStore{}
	store.calls = append(store.calls, addProviderForTenantCall{tenantID: "acme", provider: "openai"})
	store.keyCalls = append(store.keyCalls,
		providerKeyCall{tenantID: "acme", provider: "openai", key: schemas.Key{ID: "k1", Name: "primary", Value: *schemas.NewEnvVar("sk-secret")}},
	)
	h, _ := NewTenantProviderHandler(store, nil)

	ctx := newKeyCtx(t, fasthttp.MethodGet, "/api/tenants/acme/providers/openai/keys", "", "acme", "openai", "")

	h.listProviderKeys(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusOK; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	var resp struct {
		Keys []schemas.Key `json:"keys"`
	}
	if err := sonic.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Keys) != 1 {
		t.Fatalf("expected 1 key; got %d", len(resp.Keys))
	}
	if resp.Keys[0].Value.GetValue() != "REDACTED" {
		t.Fatalf("list response should redact key values; got %q", resp.Keys[0].Value.GetValue())
	}
}

func TestTenantProviderHandler_GetProviderKey_CrossTenantNotFound(t *testing.T) {
	store := &stubKeyStore{}
	store.calls = append(store.calls, addProviderForTenantCall{tenantID: "globex", provider: "openai"})
	store.keyCalls = append(store.keyCalls,
		providerKeyCall{tenantID: "globex", provider: "openai", key: schemas.Key{ID: "globex-secret", Name: "primary", Value: *schemas.NewEnvVar("sk-globex")}},
	)
	h, _ := NewTenantProviderHandler(store, nil)

	// acme should not see globex's key by id.
	ctx := newKeyCtx(t, fasthttp.MethodGet, "/api/tenants/acme/providers/openai/keys/globex-secret", "", "acme", "openai", "globex-secret")

	h.getProviderKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant get should be 404; got %d body=%s", got, string(ctx.Response.Body()))
	}
}

// stubProviderStore extension for the PUT path: capture update calls
// and back GetProviderConfigByTenant from the existing seed list.
func (s *stubProviderStore) UpdateProviderForTenant(_ context.Context, tenantID string, provider schemas.ModelProvider, config configstore.ProviderConfig, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.calls {
		if c.tenantID == tenantID && c.provider == provider {
			s.calls[i].config = config
			return nil
		}
	}
	return configstore.ErrNotFound
}

func TestTenantProviderHandler_UpdateProvider_HappyPath_Evicts(t *testing.T) {
	store := &stubProviderStore{}
	store.calls = append(store.calls, addProviderForTenantCall{
		tenantID: "acme", provider: "openai", config: configstore.ProviderConfig{
			ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: 4, BufferSize: 16},
		},
	})
	evictor := &stubEvictor{}
	h, _ := NewTenantProviderHandler(store, evictor)

	body, _ := sonic.Marshal(map[string]any{
		"network_config":              schemas.NetworkConfig{},
		"concurrency_and_buffer_size": map[string]any{"concurrency": 8, "buffer_size": 32},
	})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/providers/openai")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("provider", "openai")

	h.updateProvider(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusOK; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.calls[0].config.ConcurrencyAndBufferSize.Concurrency; got != 8 {
		t.Fatalf("concurrency should have flipped to 8; got %d", got)
	}
	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 || evictor.called[0] != multitenant.TenantID("acme") {
		t.Fatalf("Evict should fire; got %v", evictor.called)
	}
}

func TestTenantProviderHandler_UpdateProvider_NotFound(t *testing.T) {
	store := &stubProviderStore{}
	// Acme has nothing; payload targets acme/openai.
	h, _ := NewTenantProviderHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{
		"network_config":              schemas.NetworkConfig{},
		"concurrency_and_buffer_size": map[string]any{"concurrency": 4, "buffer_size": 16},
	})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/providers/openai")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("provider", "openai")

	h.updateProvider(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("missing parent should be 404; got %d body=%s", got, string(ctx.Response.Body()))
	}
}

func TestTenantProviderHandler_UpdateProvider_RejectsInvalidConcurrency(t *testing.T) {
	store := &stubProviderStore{}
	store.calls = append(store.calls, addProviderForTenantCall{tenantID: "acme", provider: "openai"})
	h, _ := NewTenantProviderHandler(store, nil)

	body, _ := sonic.Marshal(map[string]any{
		"concurrency_and_buffer_size": map[string]any{"concurrency": 32, "buffer_size": 4}, // concurrency > buffer
	})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/providers/openai")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("provider", "openai")

	h.updateProvider(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("concurrency > buffer should be 400; got %d", got)
	}
}

func TestTenantProviderHandler_DeleteProviderKey_HappyPath_Evicts(t *testing.T) {
	store := &stubKeyStore{}
	store.calls = append(store.calls, addProviderForTenantCall{tenantID: "acme", provider: "openai"})
	store.keyCalls = append(store.keyCalls,
		providerKeyCall{tenantID: "acme", provider: "openai", key: schemas.Key{ID: "k1"}},
	)
	evictor := &stubEvictor{}
	h, _ := NewTenantProviderHandler(store, evictor)

	ctx := newKeyCtx(t, fasthttp.MethodDelete, "/api/tenants/acme/providers/openai/keys/k1", "", "acme", "openai", "k1")

	h.deleteProviderKey(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusNoContent; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	if len(store.keyCalls) != 0 {
		t.Fatal("key should be gone after delete")
	}
	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 {
		t.Fatalf("Evict should fire; got %d calls", len(evictor.called))
	}
}
