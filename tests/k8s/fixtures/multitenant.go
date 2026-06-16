package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// MTAdmin drives the per-tenant admin surface that the F5XC patches add
// at /api/platform/tenants and /api/tenants/{tid}/... — endpoints the
// go-bifrost-ai SDK doesn't yet know about. Constructed via
// BifrostInstall.MTAdmin() once the install is up. All operations target
// the bifrost service via the host-side port-forward (bf.BaseURL); cross-
// replica concerns are out of scope here (covered by the propagation
// tests in scenarios/, when multi-tenant gets multi-replica scenarios).
type MTAdmin struct {
	bf *BifrostInstall
}

// MTAdmin returns the multi-tenant admin client. Safe to call regardless
// of MultiTenantEnabled; the endpoints just 404 if MT is off — which is
// itself a useful negative-test signal.
func (bf *BifrostInstall) MTAdmin() *MTAdmin { return &MTAdmin{bf: bf} }

// CreateTenant POSTs /api/platform/tenants. The platform admin picks the
// tenant ID (slug-like, stable); auto-generation would prevent IdP/SCIM
// integrations from minting tenants with stable external IDs.
func (m *MTAdmin) CreateTenant(t *testing.T, ctx context.Context, id, name string) {
	t.Helper()
	body := map[string]any{"id": id, "name": name, "status": "active"}
	resp, raw, err := m.bf.PostJSON(ctx, "/api/platform/tenants", body, nil)
	if err != nil {
		t.Fatalf("create tenant %s: %v", id, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tenant %s: status %d: %s", id, resp.StatusCode, raw)
	}
	t.Logf("created tenant %s (%s)", id, name)
}

// CreateProvider POSTs /api/tenants/{tid}/providers. Mirrors
// ConfigureMockProvider's shape but scoped to a specific tenant.
func (m *MTAdmin) CreateProvider(t *testing.T, ctx context.Context, tid, provider, baseURL string) {
	t.Helper()
	body := map[string]any{
		"provider": provider,
		"network_config": map[string]any{
			"base_url":                           baseURL,
			"default_request_timeout_in_seconds": 30,
			"max_retries":                        0,
		},
		"concurrency_and_buffer_size": map[string]any{
			"concurrency": 50,
			"buffer_size": 100,
		},
	}
	path := fmt.Sprintf("/api/tenants/%s/providers", tid)
	resp, raw, err := m.bf.PostJSON(ctx, path, body, nil)
	if err != nil {
		t.Fatalf("create provider %s for tenant %s: %v", provider, tid, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("create provider %s for tenant %s: status %d: %s", provider, tid, resp.StatusCode, raw)
	}
	t.Logf("tenant %s: created provider %s -> %s", tid, provider, baseURL)
}

// CreateProviderKey POSTs /api/tenants/{tid}/providers/{provider}/keys.
// value is a bare string (the safe shape — see governance.go).
func (m *MTAdmin) CreateProviderKey(t *testing.T, ctx context.Context, tid, provider, name, value string) {
	t.Helper()
	body := map[string]any{
		"name":  name,
		"value": value,
	}
	path := fmt.Sprintf("/api/tenants/%s/providers/%s/keys", tid, provider)
	resp, raw, err := m.bf.PostJSON(ctx, path, body, nil)
	if err != nil {
		t.Fatalf("create key for tenant %s provider %s: %v", tid, provider, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("create key for tenant %s provider %s: status %d: %s", tid, provider, resp.StatusCode, raw)
	}
	t.Logf("tenant %s: attached key %q to provider %s", tid, name, provider)
}

// CreateVirtualKey POSTs /api/tenants/{tid}/governance/virtual-keys.
// Returns the server-minted (or echoed) VK value, which the caller uses
// to drive inference via the x-bf-vk header.
func (m *MTAdmin) CreateVirtualKey(t *testing.T, ctx context.Context, tid, name string) string {
	t.Helper()
	body := map[string]any{"name": name}
	path := fmt.Sprintf("/api/tenants/%s/governance/virtual-keys", tid)
	resp, raw, err := m.bf.PostJSON(ctx, path, body, nil)
	if err != nil {
		t.Fatalf("create VK for tenant %s: %v", tid, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("create VK for tenant %s: status %d: %s", tid, resp.StatusCode, raw)
	}
	var parsed struct {
		ID       string `json:"id"`
		TenantID string `json:"tenant_id"`
		Name     string `json:"name"`
		Value    string `json:"value"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse VK response for tenant %s: %v: body=%s", tid, err, raw)
	}
	if parsed.Value == "" {
		t.Fatalf("VK response missing value for tenant %s: body=%s", tid, raw)
	}
	t.Logf("tenant %s: created VK %s (id=%s)", tid, parsed.Name, parsed.ID)
	return parsed.Value
}

// DeleteVirtualKey DELETEs /api/tenants/{tid}/governance/virtual-keys/{vk_id}.
// Used to exercise the patch-0028 invalidator wiring: after delete, the
// resolver cache should drop the entry on the calling replica, so the
// next inference call with that VK 401s instead of routing to a
// half-loaded tenant runtime.
func (m *MTAdmin) DeleteVirtualKey(t *testing.T, ctx context.Context, tid, vkID string) {
	t.Helper()
	path := fmt.Sprintf("/api/tenants/%s/governance/virtual-keys/%s", tid, vkID)
	resp, raw, err := m.delete(ctx, path)
	if err != nil {
		t.Fatalf("delete VK %s for tenant %s: %v", vkID, tid, err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete VK %s for tenant %s: status %d: %s", vkID, tid, resp.StatusCode, raw)
	}
	t.Logf("tenant %s: deleted VK %s", tid, vkID)
}

// ListVirtualKeys GETs /api/tenants/{tid}/governance/virtual-keys and
// returns the parsed (id, name, value) tuples. Used so a delete test can
// find the VK ID without the caller having to thread it through from
// create time.
func (m *MTAdmin) ListVirtualKeys(t *testing.T, ctx context.Context, tid string) []MTVirtualKey {
	t.Helper()
	path := fmt.Sprintf("/api/tenants/%s/governance/virtual-keys", tid)
	resp, raw, err := m.get(ctx, path)
	if err != nil {
		t.Fatalf("list VKs for tenant %s: %v", tid, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list VKs for tenant %s: status %d: %s", tid, resp.StatusCode, raw)
	}
	var parsed struct {
		VirtualKeys []MTVirtualKey `json:"virtual_keys"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse VK list for tenant %s: %v: body=%s", tid, err, raw)
	}
	return parsed.VirtualKeys
}

// MTVirtualKey is the slim VK shape returned by the tenant-scoped list.
// Only the fields scenarios reach for are surfaced; full schema lives in
// configstore.TableVirtualKey.
type MTVirtualKey struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Value    string `json:"value"`
}

// ChatResult is what the inference helpers report back. A non-zero
// StatusCode + non-nil Err is harness failure; StatusCode != 200 with
// nil Err is a server-side rejection (e.g. 401 after a deleted VK).
type ChatResult struct {
	StatusCode int
	Body       []byte
	Err        error
}

// Chat drives POST /v1/chat/completions with the supplied virtual key
// in the x-bf-vk header. Returns the raw status + body so scenarios can
// assert 200 vs 401/403 etc.
//
// vk is the value (sk-bf-...) returned by CreateVirtualKey, NOT the VK
// ID. Bifrost's resolver hashes the value to look up the tenant.
func (m *MTAdmin) Chat(ctx context.Context, vk, provider, model, userMessage string) ChatResult {
	body := map[string]any{
		"model": fmt.Sprintf("%s/%s", provider, model),
		"messages": []map[string]any{
			{"role": "user", "content": userMessage},
		},
		"max_tokens": 32,
	}
	headers := map[string]string{
		"x-bf-vk": vk,
	}
	resp, raw, err := m.bf.PostJSON(ctx, "/v1/chat/completions", body, headers)
	if err != nil {
		return ChatResult{Err: err}
	}
	return ChatResult{StatusCode: resp.StatusCode, Body: raw}
}

// WaitForVKResolver polls Chat until the VK either resolves successfully
// (status 200) or the deadline elapses. Provider config propagation
// across the resolver cache is asynchronous on a freshly-created VK in
// pathological cases (cold runtime + slow DB); a short bounded wait
// makes scenarios deterministic without a flat sleep.
func (m *MTAdmin) WaitForVKResolver(t *testing.T, ctx context.Context, vk, provider, model string, timeout time.Duration) ChatResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last ChatResult
	for time.Now().Before(deadline) {
		last = m.Chat(ctx, vk, provider, model, "ping")
		if last.StatusCode == http.StatusOK {
			return last
		}
		// Anything other than 401/403/404 is a real error — bail early so
		// the failure mode is visible in the log rather than buried at the
		// deadline.
		if last.StatusCode != http.StatusUnauthorized &&
			last.StatusCode != http.StatusForbidden &&
			last.StatusCode != http.StatusNotFound {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	return last
}

// delete and get factor out the parts of PostJSON that aren't POST.
// PostJSON itself can't be reused for non-POST methods without
// refactoring its public signature, which other tests depend on.
func (m *MTAdmin) delete(ctx context.Context, path string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.bf.BaseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	return m.do(req)
}

func (m *MTAdmin) get(ctx context.Context, path string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.bf.BaseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	return m.do(req)
}

func (m *MTAdmin) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := m.bf.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp, out, err
}

// SnippetOf trims a long response body for log readability. Sized for
// one-line t.Logf output; full body is still in ChatResult.Body if a
// test needs to assert on contents.
func SnippetOf(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
