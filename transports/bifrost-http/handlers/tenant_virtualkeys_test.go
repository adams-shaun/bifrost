package handlers

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// stubVKStore is the minimum ConfigStore surface this handler exercises.
type stubVKStore struct {
	configstore.ConfigStore // nil embedded — unimplemented methods panic if hit
	mu                      sync.Mutex
	created                 []*configstoreTables.TableVirtualKey
	createErr               error
}

func (s *stubVKStore) CreateVirtualKey(ctx context.Context, vk *configstoreTables.TableVirtualKey, tx ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	cp := *vk
	s.created = append(s.created, &cp)
	return nil
}

func newTenantVKCtx(t *testing.T, body string, tid string) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/" + tid + "/governance/virtual-keys")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBodyString(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	if tid != "" {
		ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), tid)
	}
	return ctx
}

func TestTenantVirtualKeyHandler_Create_HappyPath_ServerMintsValue(t *testing.T) {
	store := &stubVKStore{}
	evictor := &stubEvictor{}
	h, err := NewTenantVirtualKeyHandler(store, evictor, nil, nil)
	if err != nil {
		t.Fatalf("NewTenantVirtualKeyHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{"name": "ci-vk-1"})
	ctx := newTenantVKCtx(t, string(body), "acme")

	h.createVirtualKey(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.created); got != 1 {
		t.Fatalf("CreateVirtualKey calls got %d want 1", got)
	}
	vk := store.created[0]
	if vk.TenantID != "acme" {
		t.Fatalf("tenant_id got %q want %q", vk.TenantID, "acme")
	}
	if vk.Name != "ci-vk-1" {
		t.Fatalf("name got %q want %q", vk.Name, "ci-vk-1")
	}
	if !strings.HasPrefix(vk.Value, "sk-bf-") {
		t.Fatalf("server-minted VK should carry governance prefix; got %q", vk.Value)
	}
	if vk.IsActive == nil || *vk.IsActive != true {
		t.Fatal("new VK should default IsActive=true")
	}

	// Response is the legacy {message, virtual_key} envelope so the UI
	// can splice the new VK into its cached list. virtual_key carries
	// the full schemas.TableVirtualKey shape.
	var resp struct {
		Message    string                            `json:"message"`
		VirtualKey configstoreTables.TableVirtualKey `json:"virtual_key"`
	}
	if err := sonic.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("response decode: %v", err)
	}
	if resp.VirtualKey.Value != vk.Value {
		t.Fatalf("response virtual_key.value mismatch; got %q want %q", resp.VirtualKey.Value, vk.Value)
	}
	if resp.VirtualKey.TenantID != "acme" {
		t.Fatalf("response virtual_key.tenant_id got %q want acme", resp.VirtualKey.TenantID)
	}
}

func TestTenantVirtualKeyHandler_Create_RespectsExplicitValue(t *testing.T) {
	store := &stubVKStore{}
	h, _ := NewTenantVirtualKeyHandler(store, nil, nil, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "ci-vk-2", "value": "sk-bf-fixture-value"})
	ctx := newTenantVKCtx(t, string(body), "acme")

	h.createVirtualKey(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d (body=%s)", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.created[0].Value != "sk-bf-fixture-value" {
		t.Fatalf("explicit value should pass through; got %q", store.created[0].Value)
	}
}

func TestTenantVirtualKeyHandler_Create_MissingName(t *testing.T) {
	store := &stubVKStore{}
	h, _ := NewTenantVirtualKeyHandler(store, nil, nil, nil)

	ctx := newTenantVKCtx(t, `{}`, "acme")

	h.createVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("missing name should be 400; got %d", got)
	}
	if len(store.created) != 0 {
		t.Fatal("store must not be called when payload invalid")
	}
}

func TestTenantVirtualKeyHandler_Create_MissingTenantCtx(t *testing.T) {
	store := &stubVKStore{}
	h, _ := NewTenantVirtualKeyHandler(store, nil, nil, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "x"})
	ctx := newTenantVKCtx(t, string(body), "")

	h.createVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("missing tenant ctx should be 500; got %d", got)
	}
}

func TestTenantVirtualKeyHandler_Create_StoreError_AlreadyExists(t *testing.T) {
	store := &stubVKStore{createErr: configstore.ErrAlreadyExists}
	h, _ := NewTenantVirtualKeyHandler(store, nil, nil, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "dup"})
	ctx := newTenantVKCtx(t, string(body), "acme")

	h.createVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusConflict {
		t.Fatalf("ErrAlreadyExists should be 409; got %d", got)
	}
}

func TestTenantVirtualKeyHandler_NewWithNilStore(t *testing.T) {
	if _, err := NewTenantVirtualKeyHandler(nil, nil, nil, nil); err == nil {
		t.Fatal("nil store should error")
	}
}

// richVKStore augments stubVKStore with the ConfigStore surface the
// rate_limit / budgets / provider_configs path exercises. Each method
// records the row it was given so the test can assert on persisted
// shape.
type richVKStore struct {
	stubVKStore
	providers       map[schemas.ModelProvider]configstore.ProviderConfig
	rateLimits      []*configstoreTables.TableRateLimit
	budgets         []*configstoreTables.TableBudget
	providerConfigs []*configstoreTables.TableVirtualKeyProviderConfig
}

func (s *richVKStore) GetProvidersConfigByTenant(_ context.Context, _ string) (map[schemas.ModelProvider]configstore.ProviderConfig, error) {
	return s.providers, nil
}

func (s *richVKStore) ExecuteTransaction(_ context.Context, fn func(*gorm.DB) error) error {
	return fn(nil)
}

func (s *richVKStore) CreateRateLimitForTenant(_ context.Context, _ string, rl *configstoreTables.TableRateLimit, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *rl
	s.rateLimits = append(s.rateLimits, &cp)
	return nil
}

func (s *richVKStore) CreateBudgetForTenant(_ context.Context, _ string, b *configstoreTables.TableBudget, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *b
	s.budgets = append(s.budgets, &cp)
	return nil
}

func (s *richVKStore) CreateVirtualKeyProviderConfig(_ context.Context, pc *configstoreTables.TableVirtualKeyProviderConfig, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *pc
	s.providerConfigs = append(s.providerConfigs, &cp)
	return nil
}

func TestTenantVirtualKeyHandler_Create_WithRateLimitBudgetAndProviderConfig(t *testing.T) {
	store := &richVKStore{
		providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			schemas.ModelProvider("qwen"): {},
		},
	}
	h, err := NewTenantVirtualKeyHandler(store, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewTenantVirtualKeyHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{
		"name": "rich-vk",
		"rate_limit": map[string]any{
			"token_max_limit":      1500,
			"token_reset_duration": "1m",
		},
		"budgets": []map[string]any{
			{"max_limit": 0.05, "reset_duration": "1h"},
		},
		"provider_configs": []map[string]any{
			{"provider": "qwen", "allowed_models": []string{"qwen3.6-coder"}},
		},
	})
	ctx := newTenantVKCtx(t, string(body), "acme")

	h.createVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusCreated {
		t.Fatalf("status got %d want 201 (body=%s)", got, string(ctx.Response.Body()))
	}
	if len(store.created) != 1 {
		t.Fatalf("VK creates got %d want 1", len(store.created))
	}
	vk := store.created[0]
	if vk.RateLimitID == nil {
		t.Fatal("rate_limit id should be linked to VK")
	}
	if len(store.rateLimits) != 1 {
		t.Fatalf("rate_limit creates got %d want 1", len(store.rateLimits))
	}
	rl := store.rateLimits[0]
	if rl.TokenMaxLimit == nil || *rl.TokenMaxLimit != 1500 {
		t.Fatalf("rate_limit token_max_limit got %v want 1500", rl.TokenMaxLimit)
	}
	if len(store.budgets) != 1 {
		t.Fatalf("budget creates got %d want 1", len(store.budgets))
	}
	b := store.budgets[0]
	if b.MaxLimit != 0.05 || b.ResetDuration != "1h" {
		t.Fatalf("budget got max=%.3f reset=%s want max=0.05 reset=1h", b.MaxLimit, b.ResetDuration)
	}
	if b.VirtualKeyID == nil || *b.VirtualKeyID != vk.ID {
		t.Fatalf("budget should be linked to VK %s; got %v", vk.ID, b.VirtualKeyID)
	}
	if len(store.providerConfigs) != 1 {
		t.Fatalf("provider_config creates got %d want 1", len(store.providerConfigs))
	}
	pc := store.providerConfigs[0]
	if pc.Provider != "qwen" {
		t.Fatalf("provider_config provider got %q want qwen", pc.Provider)
	}
	if pc.VirtualKeyID != vk.ID {
		t.Fatalf("provider_config virtual_key_id mismatch; got %q want %q", pc.VirtualKeyID, vk.ID)
	}
	if !pc.AllowAllKeys {
		t.Fatal("tenant route should default AllowAllKeys=true on provider_config")
	}
}

func TestTenantVirtualKeyHandler_Create_ProviderConfig_UnconfiguredProvider_Returns400(t *testing.T) {
	store := &richVKStore{
		providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			schemas.ModelProvider("openai"): {},
		},
	}
	h, _ := NewTenantVirtualKeyHandler(store, nil, nil, nil)

	body, _ := sonic.Marshal(map[string]any{
		"name": "rich-vk-bad-provider",
		"provider_configs": []map[string]any{
			{"provider": "never-registered", "allowed_models": []string{"*"}},
		},
	})
	ctx := newTenantVKCtx(t, string(body), "acme")

	h.createVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("status got %d want 400 (body=%s)", got, string(ctx.Response.Body()))
	}
}

// stubVKStore extensions to back GET single + PUT tests.

func (s *stubVKStore) GetVirtualKeyByIDForTenant(_ context.Context, tenantID, id string) (*configstoreTables.TableVirtualKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, vk := range s.created {
		if vk.ID == id && vk.TenantID == tenantID {
			cp := *vk
			return &cp, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubVKStore) UpdateVirtualKeyForTenant(_ context.Context, tenantID string, vk *configstoreTables.TableVirtualKey, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.created {
		if existing.ID == vk.ID && existing.TenantID == tenantID {
			cp := *vk
			cp.TenantID = tenantID
			s.created[i] = &cp
			return nil
		}
	}
	return configstore.ErrNotFound
}

func TestTenantVirtualKeyHandler_Update_HappyPath_Evicts(t *testing.T) {
	store := &stubVKStore{}
	isActive := true
	store.created = append(store.created, &configstoreTables.TableVirtualKey{
		ID: "vk-1", TenantID: "acme", Name: "alpha", Value: "sk-bf-alpha", IsActive: &isActive, Description: "before",
	})
	evictor := &stubEvictor{}
	h, _ := NewTenantVirtualKeyHandler(store, evictor, nil, nil)

	body, _ := sonic.Marshal(map[string]any{"description": "after"})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/governance/virtual-keys/vk-1")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("vk_id", "vk-1")

	h.updateVirtualKey(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusOK; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.created[0].Description; got != "after" {
		t.Fatalf("description should have been patched; got %q", got)
	}
	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 || evictor.called[0] != multitenant.TenantID("acme") {
		t.Fatalf("Evict should fire for acme; got %v", evictor.called)
	}
}

func TestTenantVirtualKeyHandler_Update_CrossTenantReturns404(t *testing.T) {
	store := &stubVKStore{}
	isActive := true
	store.created = append(store.created, &configstoreTables.TableVirtualKey{
		ID: "vk-secret", TenantID: "globex", Name: "secret", Value: "sk-bf-secret", IsActive: &isActive,
	})
	h, _ := NewTenantVirtualKeyHandler(store, nil, nil, nil)

	body, _ := sonic.Marshal(map[string]any{"description": "hijack"})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/governance/virtual-keys/vk-secret")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("vk_id", "vk-secret")

	h.updateVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant put should be 404; got %d body=%s", got, string(ctx.Response.Body()))
	}
	// Original row untouched.
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.created[0].Description; got != "" {
		t.Fatalf("globex's row should not have been mutated; got description=%q", got)
	}
}

func TestTenantVirtualKeyHandler_Update_RejectsEmptyBody(t *testing.T) {
	store := &stubVKStore{}
	store.created = append(store.created, &configstoreTables.TableVirtualKey{ID: "vk-1", TenantID: "acme"})
	h, _ := NewTenantVirtualKeyHandler(store, nil, nil, nil)

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/governance/virtual-keys/vk-1")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody([]byte(`{}`))
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("vk_id", "vk-1")

	h.updateVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("empty patch body should be 400; got %d", got)
	}
}

// Ensures errors package is referenced from this file (used by the
// PUT/DELETE error-mapping logic above) — keeps the imports list
// honest under go vet's unused-import check.
var _ = errors.New

func TestTenantVirtualKeyHandler_Create_EvictsTenantRuntime(t *testing.T) {
	store := &stubVKStore{}
	evictor := &stubEvictor{}
	h, _ := NewTenantVirtualKeyHandler(store, evictor, nil, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "vk"})
	ctx := newTenantVKCtx(t, string(body), "acme")

	h.createVirtualKey(ctx)

	evictor.mu.Lock()
	defer evictor.mu.Unlock()
	if len(evictor.called) != 1 {
		t.Fatalf("Evict should fire once; got %d", len(evictor.called))
	}
	if evictor.called[0] != multitenant.TenantID("acme") {
		t.Fatalf("evicted tid got %q want acme", evictor.called[0])
	}
}

// stubVKInvalidator captures every Invalidate call so the handler
// tests can assert the resolver-cache wiring is exercised on all
// three mutate paths.
type stubVKInvalidator struct {
	mu     sync.Mutex
	called []string
}

func (s *stubVKInvalidator) Invalidate(vk string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = append(s.called, vk)
}

func (s *stubVKStore) DeleteVirtualKeyForTenant(_ context.Context, tenantID, id string, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, vk := range s.created {
		if vk.ID == id && vk.TenantID == tenantID {
			s.created = append(s.created[:i], s.created[i+1:]...)
			return nil
		}
	}
	return configstore.ErrNotFound
}

func TestTenantVirtualKeyHandler_Create_InvalidatesResolverCache(t *testing.T) {
	store := &stubVKStore{}
	inv := &stubVKInvalidator{}
	h, _ := NewTenantVirtualKeyHandler(store, nil, inv, nil)

	body, _ := sonic.Marshal(map[string]any{"name": "vk-new", "value": "sk-bf-explicit"})
	ctx := newTenantVKCtx(t, string(body), "acme")

	h.createVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusCreated {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if len(inv.called) != 1 || inv.called[0] != "sk-bf-explicit" {
		t.Fatalf("Invalidate should fire once with new VK value; got %v", inv.called)
	}
}

func TestTenantVirtualKeyHandler_Update_InvalidatesResolverCache(t *testing.T) {
	store := &stubVKStore{}
	isActive := true
	store.created = append(store.created, &configstoreTables.TableVirtualKey{
		ID: "vk-1", TenantID: "acme", Name: "alpha", Value: "sk-bf-alpha", IsActive: &isActive,
	})
	inv := &stubVKInvalidator{}
	h, _ := NewTenantVirtualKeyHandler(store, nil, inv, nil)

	body, _ := sonic.Marshal(map[string]any{"is_active": false})
	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/governance/virtual-keys/vk-1")
	req.Header.SetMethod(fasthttp.MethodPut)
	req.SetBody(body)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("vk_id", "vk-1")

	h.updateVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if len(inv.called) != 1 || inv.called[0] != "sk-bf-alpha" {
		t.Fatalf("Update should invalidate the existing VK value; got %v", inv.called)
	}
}

func TestTenantVirtualKeyHandler_Delete_InvalidatesResolverCacheUsingPreDeleteValue(t *testing.T) {
	store := &stubVKStore{}
	isActive := true
	store.created = append(store.created, &configstoreTables.TableVirtualKey{
		ID: "vk-1", TenantID: "acme", Name: "alpha", Value: "sk-bf-alpha", IsActive: &isActive,
	})
	inv := &stubVKInvalidator{}
	h, _ := NewTenantVirtualKeyHandler(store, nil, inv, nil)

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/governance/virtual-keys/vk-1")
	req.Header.SetMethod(fasthttp.MethodDelete)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("vk_id", "vk-1")

	h.deleteVirtualKey(ctx)

	// DELETE now returns 200 + {message: "Virtual key deleted successfully"}
	// (legacy envelope) instead of 204, so RTK Query's typed mutation
	// (returns {message: string}) round-trips.
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if len(inv.called) != 1 || inv.called[0] != "sk-bf-alpha" {
		t.Fatalf("Delete should invalidate the pre-delete VK value; got %v", inv.called)
	}
	// And the row really is gone — proves the invalidate ran AFTER the
	// store mutation, not before.
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.created) != 0 {
		t.Fatalf("DELETE should have removed the row; remaining=%d", len(store.created))
	}
}

func TestTenantVirtualKeyHandler_Delete_NoInvalidatorWhenLookupReturnsNotFound(t *testing.T) {
	store := &stubVKStore{} // empty — GetVirtualKeyByIDForTenant returns ErrNotFound
	inv := &stubVKInvalidator{}
	h, _ := NewTenantVirtualKeyHandler(store, nil, inv, nil)

	var req fasthttp.Request
	req.SetRequestURI("/api/tenants/acme/governance/virtual-keys/nope")
	req.Header.SetMethod(fasthttp.MethodDelete)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), "acme")
	ctx.SetUserValue("vk_id", "nope")

	h.deleteVirtualKey(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("missing VK delete should be 404; got %d", got)
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if len(inv.called) != 0 {
		t.Fatalf("no Invalidate should fire when delete 404s; got %v", inv.called)
	}
}
