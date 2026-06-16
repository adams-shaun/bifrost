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

// stubTeamsStore is the minimum ConfigStore surface this handler needs.
type stubTeamsStore struct {
	configstore.ConfigStore // unimplemented methods panic
	mu                      sync.Mutex
	teams                   []*configstoreTables.TableTeam
	createErr               error
}

func (s *stubTeamsStore) CreateTeamForTenant(_ context.Context, tenantID string, team *configstoreTables.TableTeam, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	cp := *team
	cp.TenantID = tenantID
	s.teams = append(s.teams, &cp)
	return nil
}

func (s *stubTeamsStore) GetTeamsByTenant(_ context.Context, tenantID, customerID string) ([]configstoreTables.TableTeam, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []configstoreTables.TableTeam
	for _, t := range s.teams {
		if t.TenantID != tenantID {
			continue
		}
		if customerID != "" && (t.CustomerID == nil || *t.CustomerID != customerID) {
			continue
		}
		out = append(out, *t)
	}
	return out, nil
}

func (s *stubTeamsStore) GetTeamByIDForTenant(_ context.Context, tenantID, id string) (*configstoreTables.TableTeam, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.teams {
		if t.ID == id && t.TenantID == tenantID {
			cp := *t
			return &cp, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func (s *stubTeamsStore) UpdateTeamForTenant(_ context.Context, tenantID string, team *configstoreTables.TableTeam, _ ...*gorm.DB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.teams {
		if t.ID == team.ID && t.TenantID == tenantID {
			cp := *team
			cp.TenantID = tenantID
			s.teams[i] = &cp
			return nil
		}
	}
	return configstore.ErrNotFound
}

func (s *stubTeamsStore) DeleteTeamForTenant(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.teams {
		if t.ID == id && t.TenantID == tenantID {
			s.teams = append(s.teams[:i], s.teams[i+1:]...)
			return nil
		}
	}
	return configstore.ErrNotFound
}

func newTeamCtx(t *testing.T, method, path, body, tid, teamID string) *fasthttp.RequestCtx {
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
	if teamID != "" {
		ctx.SetUserValue("team_id", teamID)
	}
	return ctx
}

func TestTenantTeamsHandler_Create_HappyPath_MintsID(t *testing.T) {
	store := &stubTeamsStore{}
	h, err := NewTenantTeamsHandler(store)
	if err != nil {
		t.Fatalf("NewTenantTeamsHandler: %v", err)
	}

	body, _ := sonic.Marshal(map[string]any{"name": "engineering"})
	ctx := newTeamCtx(t, fasthttp.MethodPost, "/api/tenants/acme/governance/teams", string(body), "acme", "")

	h.createTeam(ctx)

	if got, want := ctx.Response.StatusCode(), fasthttp.StatusCreated; got != want {
		t.Fatalf("status got %d want %d body=%s", got, want, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.teams) != 1 || store.teams[0].Name != "engineering" {
		t.Fatalf("team should have been created with name engineering; got %+v", store.teams)
	}
	if store.teams[0].ID == "" {
		t.Fatal("server should mint id when omitted")
	}
	if store.teams[0].TenantID != "acme" {
		t.Fatalf("tenant_id got %q want acme", store.teams[0].TenantID)
	}
}

func TestTenantTeamsHandler_List_FiltersByTenant(t *testing.T) {
	store := &stubTeamsStore{}
	store.teams = append(store.teams,
		&configstoreTables.TableTeam{ID: "t-a", Name: "acme-eng", TenantID: "acme"},
		&configstoreTables.TableTeam{ID: "t-a2", Name: "acme-platform", TenantID: "acme"},
		&configstoreTables.TableTeam{ID: "t-g", Name: "globex-eng", TenantID: "globex"},
	)
	h, _ := NewTenantTeamsHandler(store)

	ctx := newTeamCtx(t, fasthttp.MethodGet, "/api/tenants/acme/governance/teams", "", "acme", "")

	h.listTeams(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	// List response matches GetTeamsResponse — {teams, count,
	// total_count, limit, offset}. The previous {tenant_id, teams,
	// total} envelope is gone (UI's RTK Query cache patches read
	// response.teams + response.count).
	var resp struct {
		Teams      []configstoreTables.TableTeam `json:"teams"`
		Count      int                           `json:"count"`
		TotalCount int                           `json:"total_count"`
	}
	if err := sonic.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 2 {
		t.Fatalf("acme should see 2 teams; got %d", resp.Count)
	}
}

func TestTenantTeamsHandler_Get_CrossTenant_NotFound(t *testing.T) {
	store := &stubTeamsStore{}
	store.teams = append(store.teams, &configstoreTables.TableTeam{ID: "t-globex", TenantID: "globex"})
	h, _ := NewTenantTeamsHandler(store)

	ctx := newTeamCtx(t, fasthttp.MethodGet, "/api/tenants/acme/governance/teams/t-globex", "", "acme", "t-globex")

	h.getTeam(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant GET should be 404; got %d", got)
	}
}

func TestTenantTeamsHandler_Update_HappyPath(t *testing.T) {
	store := &stubTeamsStore{}
	store.teams = append(store.teams, &configstoreTables.TableTeam{ID: "t-1", Name: "before", TenantID: "acme"})
	h, _ := NewTenantTeamsHandler(store)

	body, _ := sonic.Marshal(map[string]any{"name": "after"})
	ctx := newTeamCtx(t, fasthttp.MethodPut, "/api/tenants/acme/governance/teams/t-1", string(body), "acme", "t-1")

	h.updateTeam(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.teams[0].Name != "after" {
		t.Fatalf("name should have flipped; got %q", store.teams[0].Name)
	}
}

func TestTenantTeamsHandler_Update_RejectsEmpty(t *testing.T) {
	store := &stubTeamsStore{}
	store.teams = append(store.teams, &configstoreTables.TableTeam{ID: "t-1", TenantID: "acme"})
	h, _ := NewTenantTeamsHandler(store)

	ctx := newTeamCtx(t, fasthttp.MethodPut, "/api/tenants/acme/governance/teams/t-1", `{}`, "acme", "t-1")

	h.updateTeam(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadRequest {
		t.Fatalf("empty patch should be 400; got %d", got)
	}
}

func TestTenantTeamsHandler_Delete_HappyPath(t *testing.T) {
	store := &stubTeamsStore{}
	store.teams = append(store.teams, &configstoreTables.TableTeam{ID: "t-1", TenantID: "acme"})
	h, _ := NewTenantTeamsHandler(store)

	ctx := newTeamCtx(t, fasthttp.MethodDelete, "/api/tenants/acme/governance/teams/t-1", "", "acme", "t-1")

	h.deleteTeam(ctx)

	// DELETE returns 200 + {message} (legacy envelope) instead of 204.
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status got %d body=%s", got, string(ctx.Response.Body()))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.teams) != 0 {
		t.Fatal("team should be gone")
	}
}

func TestTenantTeamsHandler_Delete_CrossTenant_NotFound(t *testing.T) {
	store := &stubTeamsStore{}
	store.teams = append(store.teams, &configstoreTables.TableTeam{ID: "t-globex", TenantID: "globex"})
	h, _ := NewTenantTeamsHandler(store)

	ctx := newTeamCtx(t, fasthttp.MethodDelete, "/api/tenants/acme/governance/teams/t-globex", "", "acme", "t-globex")

	h.deleteTeam(ctx)

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("cross-tenant DELETE should be 404; got %d", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.teams) != 1 {
		t.Fatal("globex's team should survive a cross-tenant delete attempt")
	}
}

func TestTenantTeamsHandler_NewWithNilStore(t *testing.T) {
	if _, err := NewTenantTeamsHandler(nil); err == nil {
		t.Fatal("nil store should error")
	}
}
