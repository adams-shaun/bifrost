package handlers

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// ---- Budgets ----

type stubBudgetsStore struct {
	configstore.ConfigStore
	mu      sync.Mutex
	budgets []*configstoreTables.TableBudget
}

func (s *stubBudgetsStore) CreateBudgetForTenant(_ context.Context, tenantID string, b *configstoreTables.TableBudget, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *b
	cp.TenantID = tenantID
	s.budgets = append(s.budgets, &cp)
	return nil
}

func (s *stubBudgetsStore) GetBudgetsByTenant(_ context.Context, tenantID string) ([]configstoreTables.TableBudget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []configstoreTables.TableBudget
	for _, b := range s.budgets {
		if b.TenantID == tenantID {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (s *stubBudgetsStore) GetBudgetForTenant(_ context.Context, tenantID, id string) (*configstoreTables.TableBudget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.budgets {
		if b.ID == id && b.TenantID == tenantID {
			cp := *b
			return &cp, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubBudgetsStore) UpdateBudgetForTenant(_ context.Context, tenantID string, b *configstoreTables.TableBudget, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.budgets {
		if existing.ID == b.ID && existing.TenantID == tenantID {
			cp := *b
			cp.TenantID = tenantID
			s.budgets[i] = &cp
			return nil
		}
	}
	return configstore.ErrNotFound
}

func (s *stubBudgetsStore) DeleteBudgetForTenant(_ context.Context, tenantID, id string, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.budgets {
		if b.ID == id && b.TenantID == tenantID {
			s.budgets = append(s.budgets[:i], s.budgets[i+1:]...)
			return nil
		}
	}
	return configstore.ErrNotFound
}

func newBudgetCtx(t *testing.T, method, path, body, tid, budgetID string) *fasthttp.RequestCtx {
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
	if budgetID != "" {
		ctx.SetUserValue("budget_id", budgetID)
	}
	return ctx
}

func TestTenantBudgetsHandler_Create_HappyPath(t *testing.T) {
	store := &stubBudgetsStore{}
	h, err := NewTenantBudgetsHandler(store)
	if err != nil {
		t.Fatalf("NewTenantBudgetsHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{"max_limit": 100.0, "reset_duration": "1d"})
	ctx := newBudgetCtx(t, fasthttp.MethodPost, "/api/tenants/acme/governance/budgets", string(body), "acme", "")

	h.create(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusCreated {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.budgets) != 1 || store.budgets[0].MaxLimit != 100 {
		t.Fatalf("budget should have been created; got %+v", store.budgets)
	}
	if store.budgets[0].TenantID != "acme" {
		t.Fatalf("tenant_id got %q want acme", store.budgets[0].TenantID)
	}
}

func TestTenantBudgetsHandler_Create_RejectsNegativeLimit(t *testing.T) {
	store := &stubBudgetsStore{}
	h, _ := NewTenantBudgetsHandler(store)

	body, _ := sonic.Marshal(map[string]any{"max_limit": -1.0, "reset_duration": "1d"})
	ctx := newBudgetCtx(t, fasthttp.MethodPost, "/api/tenants/acme/governance/budgets", string(body), "acme", "")

	h.create(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("negative max_limit should be 400; got %d", got)
	}
}

func TestTenantBudgetsHandler_List_FiltersByTenant(t *testing.T) {
	store := &stubBudgetsStore{}
	store.budgets = append(store.budgets,
		&configstoreTables.TableBudget{ID: "b1", TenantID: "acme", MaxLimit: 100, ResetDuration: "1d"},
		&configstoreTables.TableBudget{ID: "b2", TenantID: "globex", MaxLimit: 200, ResetDuration: "1d"},
	)
	h, _ := NewTenantBudgetsHandler(store)

	ctx := newBudgetCtx(t, fasthttp.MethodGet, "/api/tenants/acme/governance/budgets", "", "acme", "")
	h.list(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	var resp struct {
		Count int `json:"count"`
	}
	_ = sonic.Unmarshal(ctx.Response.Body(), &resp)
	if resp.Count != 1 {
		t.Fatalf("acme should see 1 budget; got %d", resp.Count)
	}
}

func TestTenantBudgetsHandler_Update_HappyPath(t *testing.T) {
	store := &stubBudgetsStore{}
	store.budgets = append(store.budgets, &configstoreTables.TableBudget{ID: "b1", TenantID: "acme", MaxLimit: 100, ResetDuration: "1d"})
	h, _ := NewTenantBudgetsHandler(store)

	body, _ := sonic.Marshal(map[string]any{"max_limit": 200.0})
	ctx := newBudgetCtx(t, fasthttp.MethodPut, "/api/tenants/acme/governance/budgets/b1", string(body), "acme", "b1")
	h.update(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.budgets[0].MaxLimit != 200 {
		t.Fatalf("max_limit should be 200; got %v", store.budgets[0].MaxLimit)
	}
}

func TestTenantBudgetsHandler_Delete_CrossTenant_NotFound(t *testing.T) {
	store := &stubBudgetsStore{}
	store.budgets = append(store.budgets, &configstoreTables.TableBudget{ID: "bg", TenantID: "globex"})
	h, _ := NewTenantBudgetsHandler(store)

	ctx := newBudgetCtx(t, fasthttp.MethodDelete, "/api/tenants/acme/governance/budgets/bg", "", "acme", "bg")
	h.delete(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant DELETE should be 404; got %d", got)
	}
}

func TestTenantBudgetsHandler_NewWithNilStore(t *testing.T) {
	if _, err := NewTenantBudgetsHandler(nil); err == nil {
		t.Fatal("nil store should error")
	}
}

// ---- Rate limits ----

type stubRateLimitsStore struct {
	configstore.ConfigStore
	mu  sync.Mutex
	rls []*configstoreTables.TableRateLimit
}

func (s *stubRateLimitsStore) CreateRateLimitForTenant(_ context.Context, tenantID string, rl *configstoreTables.TableRateLimit, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *rl
	cp.TenantID = tenantID
	s.rls = append(s.rls, &cp)
	return nil
}

func (s *stubRateLimitsStore) GetRateLimitsByTenant(_ context.Context, tenantID string) ([]configstoreTables.TableRateLimit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []configstoreTables.TableRateLimit
	for _, r := range s.rls {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubRateLimitsStore) GetRateLimitForTenant(_ context.Context, tenantID, id string) (*configstoreTables.TableRateLimit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rls {
		if r.ID == id && r.TenantID == tenantID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubRateLimitsStore) UpdateRateLimitForTenant(_ context.Context, tenantID string, rl *configstoreTables.TableRateLimit, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.rls {
		if existing.ID == rl.ID && existing.TenantID == tenantID {
			cp := *rl
			cp.TenantID = tenantID
			s.rls[i] = &cp
			return nil
		}
	}
	return configstore.ErrNotFound
}

func (s *stubRateLimitsStore) DeleteRateLimitForTenant(_ context.Context, tenantID, id string, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rls {
		if r.ID == id && r.TenantID == tenantID {
			s.rls = append(s.rls[:i], s.rls[i+1:]...)
			return nil
		}
	}
	return configstore.ErrNotFound
}

func newRLCtx(t *testing.T, method, path, body, tid, rlID string) *fasthttp.RequestCtx {
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
	if rlID != "" {
		ctx.SetUserValue("rate_limit_id", rlID)
	}
	return ctx
}

func TestTenantRateLimitsHandler_Create_HappyPath(t *testing.T) {
	store := &stubRateLimitsStore{}
	h, err := NewTenantRateLimitsHandler(store)
	if err != nil {
		t.Fatalf("NewTenantRateLimitsHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{"token_max_limit": 1000, "token_reset_duration": "1m"})
	ctx := newRLCtx(t, fasthttp.MethodPost, "/api/tenants/acme/governance/rate-limits", string(body), "acme", "")
	h.create(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusCreated {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.rls) != 1 || store.rls[0].TenantID != "acme" {
		t.Fatalf("rate limit should have been created with tenant_id acme; got %+v", store.rls)
	}
}

func TestTenantRateLimitsHandler_Create_RejectsEmptyLimits(t *testing.T) {
	store := &stubRateLimitsStore{}
	h, _ := NewTenantRateLimitsHandler(store)

	body, _ := sonic.Marshal(map[string]any{}) // neither token nor request limit
	ctx := newRLCtx(t, fasthttp.MethodPost, "/api/tenants/acme/governance/rate-limits", string(body), "acme", "")
	h.create(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("empty limits should be 400; got %d", got)
	}
}

func TestTenantRateLimitsHandler_Get_CrossTenant_NotFound(t *testing.T) {
	store := &stubRateLimitsStore{}
	store.rls = append(store.rls, &configstoreTables.TableRateLimit{ID: "rl-g", TenantID: "globex"})
	h, _ := NewTenantRateLimitsHandler(store)

	ctx := newRLCtx(t, fasthttp.MethodGet, "/api/tenants/acme/governance/rate-limits/rl-g", "", "acme", "rl-g")
	h.get(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant GET should be 404; got %d", got)
	}
}

func TestTenantRateLimitsHandler_Update_HappyPath(t *testing.T) {
	store := &stubRateLimitsStore{}
	tokenLimit := int64(1000)
	store.rls = append(store.rls, &configstoreTables.TableRateLimit{ID: "rl-1", TenantID: "acme", TokenMaxLimit: &tokenLimit})
	h, _ := NewTenantRateLimitsHandler(store)

	body, _ := sonic.Marshal(map[string]any{"token_max_limit": 2000})
	ctx := newRLCtx(t, fasthttp.MethodPut, "/api/tenants/acme/governance/rate-limits/rl-1", string(body), "acme", "rl-1")
	h.update(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.rls[0].TokenMaxLimit == nil || *store.rls[0].TokenMaxLimit != 2000 {
		t.Fatalf("token_max_limit should be 2000; got %+v", store.rls[0].TokenMaxLimit)
	}
}

func TestTenantRateLimitsHandler_Delete_CrossTenant_NotFound(t *testing.T) {
	store := &stubRateLimitsStore{}
	store.rls = append(store.rls, &configstoreTables.TableRateLimit{ID: "rl-g", TenantID: "globex"})
	h, _ := NewTenantRateLimitsHandler(store)

	ctx := newRLCtx(t, fasthttp.MethodDelete, "/api/tenants/acme/governance/rate-limits/rl-g", "", "acme", "rl-g")
	h.delete(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant DELETE should be 404; got %d", got)
	}
}

func TestTenantRateLimitsHandler_NewWithNilStore(t *testing.T) {
	if _, err := NewTenantRateLimitsHandler(nil); err == nil {
		t.Fatal("nil store should error")
	}
}
