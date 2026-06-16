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

type stubCustomersStore struct {
	configstore.ConfigStore
	mu        sync.Mutex
	customers []*configstoreTables.TableCustomer
}

func (s *stubCustomersStore) CreateCustomerForTenant(_ context.Context, tenantID string, c *configstoreTables.TableCustomer, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	cp.TenantID = tenantID
	s.customers = append(s.customers, &cp)
	return nil
}

func (s *stubCustomersStore) GetCustomersByTenant(_ context.Context, tenantID string) ([]configstoreTables.TableCustomer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []configstoreTables.TableCustomer
	for _, c := range s.customers {
		if c.TenantID == tenantID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (s *stubCustomersStore) GetCustomerByIDForTenant(_ context.Context, tenantID, id string) (*configstoreTables.TableCustomer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.customers {
		if c.ID == id && c.TenantID == tenantID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubCustomersStore) UpdateCustomerForTenant(_ context.Context, tenantID string, c *configstoreTables.TableCustomer, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.customers {
		if existing.ID == c.ID && existing.TenantID == tenantID {
			cp := *c
			cp.TenantID = tenantID
			s.customers[i] = &cp
			return nil
		}
	}
	return configstore.ErrNotFound
}

func (s *stubCustomersStore) DeleteCustomerForTenant(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.customers {
		if c.ID == id && c.TenantID == tenantID {
			s.customers = append(s.customers[:i], s.customers[i+1:]...)
			return nil
		}
	}
	return configstore.ErrNotFound
}

func newCustomerCtx(t *testing.T, method, path, body, tid, customerID string) *fasthttp.RequestCtx {
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
	if customerID != "" {
		ctx.SetUserValue("customer_id", customerID)
	}
	return ctx
}

func TestTenantCustomersHandler_Create_HappyPath_MintsID(t *testing.T) {
	store := &stubCustomersStore{}
	h, err := NewTenantCustomersHandler(store)
	if err != nil {
		t.Fatalf("NewTenantCustomersHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{"name": "Acme Inc"})
	ctx := newCustomerCtx(t, fasthttp.MethodPost, "/api/tenants/acme/governance/customers", string(body), "acme", "")

	h.createCustomer(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.customers) != 1 || store.customers[0].Name != "Acme Inc" {
		t.Fatalf("customer should have been created; got %+v", store.customers)
	}
	if store.customers[0].ID == "" {
		t.Fatal("server should mint id when omitted")
	}
	if store.customers[0].TenantID != "acme" {
		t.Fatalf("tenant_id got %q want acme", store.customers[0].TenantID)
	}
}

func TestTenantCustomersHandler_List_FiltersByTenant(t *testing.T) {
	store := &stubCustomersStore{}
	store.customers = append(store.customers,
		&configstoreTables.TableCustomer{ID: "c-1", Name: "Acme", TenantID: "acme"},
		&configstoreTables.TableCustomer{ID: "c-2", Name: "Globex", TenantID: "globex"},
	)
	h, _ := NewTenantCustomersHandler(store)

	ctx := newCustomerCtx(t, fasthttp.MethodGet, "/api/tenants/acme/governance/customers", "", "acme", "")

	h.listCustomers(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	// List response matches GetCustomersResponse — {customers, count, ...}.
	var resp struct {
		Customers []configstoreTables.TableCustomer `json:"customers"`
		Count     int                               `json:"count"`
	}
	if err := sonic.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 || resp.Customers[0].Name != "Acme" {
		t.Fatalf("acme should see one customer (Acme); got %+v", resp.Customers)
	}
}

func TestTenantCustomersHandler_Get_CrossTenant_NotFound(t *testing.T) {
	store := &stubCustomersStore{}
	store.customers = append(store.customers, &configstoreTables.TableCustomer{ID: "c-g", TenantID: "globex"})
	h, _ := NewTenantCustomersHandler(store)

	ctx := newCustomerCtx(t, fasthttp.MethodGet, "/api/tenants/acme/governance/customers/c-g", "", "acme", "c-g")

	h.getCustomer(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant GET should be 404; got %d", got)
	}
}

func TestTenantCustomersHandler_Update_HappyPath(t *testing.T) {
	store := &stubCustomersStore{}
	store.customers = append(store.customers, &configstoreTables.TableCustomer{ID: "c-1", Name: "before", TenantID: "acme"})
	h, _ := NewTenantCustomersHandler(store)

	body, _ := sonic.Marshal(map[string]any{"name": "after"})
	ctx := newCustomerCtx(t, fasthttp.MethodPut, "/api/tenants/acme/governance/customers/c-1", string(body), "acme", "c-1")

	h.updateCustomer(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.customers[0].Name != "after" {
		t.Fatalf("name should have flipped; got %q", store.customers[0].Name)
	}
}

func TestTenantCustomersHandler_Delete_HappyPath(t *testing.T) {
	store := &stubCustomersStore{}
	store.customers = append(store.customers, &configstoreTables.TableCustomer{ID: "c-1", TenantID: "acme"})
	h, _ := NewTenantCustomersHandler(store)

	ctx := newCustomerCtx(t, fasthttp.MethodDelete, "/api/tenants/acme/governance/customers/c-1", "", "acme", "c-1")

	h.deleteCustomer(ctx)

	// DELETE returns 200 + {message} (legacy envelope) instead of 204.
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d", got)
	}
}

func TestTenantCustomersHandler_Delete_CrossTenant_NotFound(t *testing.T) {
	store := &stubCustomersStore{}
	store.customers = append(store.customers, &configstoreTables.TableCustomer{ID: "c-g", TenantID: "globex"})
	h, _ := NewTenantCustomersHandler(store)

	ctx := newCustomerCtx(t, fasthttp.MethodDelete, "/api/tenants/acme/governance/customers/c-g", "", "acme", "c-g")

	h.deleteCustomer(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant DELETE should be 404; got %d", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.customers) != 1 {
		t.Fatal("globex's customer should survive cross-tenant delete attempt")
	}
}

func TestTenantCustomersHandler_NewWithNilStore(t *testing.T) {
	if _, err := NewTenantCustomersHandler(nil); err == nil {
		t.Fatal("nil store should error")
	}
}
