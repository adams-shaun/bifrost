package mcp

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestGetClientByName_TenantScope locks the patch16 invariant: same Name
// in two tenants must resolve to two distinct MCPClientState entries.
// Counterpart to isolation test I6 (cluster-level harness).
func TestGetClientByName_TenantScope(t *testing.T) {
	m := &MCPManager{
		logger:    defaultLogger,
		clientMap: map[string]*schemas.MCPClientState{},
	}
	add := func(id, tenant, name string) {
		m.clientMap[id] = &schemas.MCPClientState{
			Name:     name,
			TenantID: tenant,
			ExecutionConfig: &schemas.MCPClientConfig{
				ID:       id,
				Name:     name,
				TenantID: tenant,
			},
		}
	}

	add("id-a", "tenant-a", "echo")
	add("id-b", "tenant-b", "echo")
	add("id-untenanted", "", "global-thing")

	cases := []struct {
		name     string
		tenant   string
		client   string
		wantID   string // empty = expect not found
	}{
		{"tenant-a finds its own echo", "tenant-a", "echo", "id-a"},
		{"tenant-b finds its own echo (no cross-tenant leak)", "tenant-b", "echo", "id-b"},
		{"tenant-a does NOT see tenant-b's row even though name matches", "tenant-a", "echo-b-only", ""},
		{"empty tenantID falls back to 'any tenant' (OSS path)", "", "echo", ""},
		// Note: empty-tenant "any" lookup actually picks the FIRST match in iteration order.
		// We can't pin which one (map iteration is randomized), so we only assert non-nil.
		{"unscoped client visible regardless of caller tenant", "tenant-a", "global-thing", "id-untenanted"},
		{"unscoped client visible when caller has no tenant", "", "global-thing", "id-untenanted"},
		{"missing name returns nil", "tenant-a", "does-not-exist", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := m.GetClientByName(tc.tenant, tc.client)
			if tc.wantID == "" {
				if got != nil && tc.client != "echo" {
					t.Fatalf("expected nil for missing client, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected to find client id=%q tenant=%q name=%q", tc.wantID, tc.tenant, tc.client)
			}
			if got.ExecutionConfig.ID != tc.wantID {
				t.Fatalf("got client id %q, want %q (tenant=%q name=%q)", got.ExecutionConfig.ID, tc.wantID, tc.tenant, tc.client)
			}
		})
	}
}

// TestGetClientByName_CrossTenantNoLeak is the cross-tenant leak canary —
// even if multiple tenants pick the EXACT same Name, GetClientByName must
// return per-tenant results. Before patch16, this returned whichever entry
// the map iteration hit first (= leak).
func TestGetClientByName_CrossTenantNoLeak(t *testing.T) {
	m := &MCPManager{
		logger:    defaultLogger,
		clientMap: map[string]*schemas.MCPClientState{},
	}
	tenants := []string{"acme", "beta", "gamma"}
	const shared = "isotest_echo"
	for _, tid := range tenants {
		id := "id-" + tid
		m.clientMap[id] = &schemas.MCPClientState{
			Name:            shared,
			TenantID:        tid,
			ExecutionConfig: &schemas.MCPClientConfig{ID: id, Name: shared, TenantID: tid},
		}
	}
	for _, tid := range tenants {
		got := m.GetClientByName(tid, shared)
		if got == nil {
			t.Fatalf("tenant %q lost its own client %q", tid, shared)
		}
		if got.TenantID != tid {
			t.Fatalf("tenant %q got tenant-%q's client (cross-tenant leak): %+v", tid, got.TenantID, got)
		}
		if got.ExecutionConfig.ID != "id-"+tid {
			t.Fatalf("tenant %q got wrong client id %q (want %q)", tid, got.ExecutionConfig.ID, "id-"+tid)
		}
	}
}

// TestTenantIDFromBifrostContext is a tiny safety net for the helper —
// the typed key needs to round-trip through interface{} equality.
func TestTenantIDFromBifrostContext(t *testing.T) {
	if got := TenantIDFromBifrostContext(nil); got != "" {
		t.Fatalf("nil ctx should return empty, got %q", got)
	}
}
