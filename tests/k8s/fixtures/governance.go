package fixtures

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ConfigureMockProvider registers the in-cluster mock LLM with Bifrost as an
// openai-compatible provider. Provider creation and key creation are TWO
// separate API calls — POST /api/providers ignores `keys` in the payload, so
// without the second call openai has zero keys and inference returns 403.
//
// This also exercises the provider + provider-key APIs (part of the sanity
// surface) and must run before a VirtualKey references the "openai" provider.
func (bf *BifrostInstall) ConfigureMockProvider(t *testing.T) {
	t.Helper()
	if bf.MockLLM == nil {
		t.Fatalf("ConfigureMockProvider requires a deployed mock LLM")
	}

	// 1) Create the provider (no keys field; it's ignored here).
	provider := map[string]any{
		"provider": "openai",
		"network_config": map[string]any{
			"base_url":                           bf.MockLLM.InClusterURL,
			"default_request_timeout_in_seconds": 30,
			"max_retries":                        0,
		},
		"concurrency_and_buffer_size": map[string]any{
			"concurrency": 50,
			"buffer_size": 100,
		},
	}
	resp, raw, err := bf.PostJSON(context.Background(), "/api/providers", provider, nil)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("create provider: status %d: %s", resp.StatusCode, raw)
	}

	// 2) Attach a key. value is a bare string (the safe shape: a partial inline
	// object is stored as a literal and never matches). models=["*"] grants
	// access to every model in the catalog.
	key := map[string]any{
		"name":    "mock",
		"value":   "mock-key",
		"models":  []string{"*"},
		"weight":  1,
		"enabled": true,
	}
	resp, raw, err = bf.PostJSON(context.Background(), "/api/providers/openai/keys", key, nil)
	if err != nil {
		t.Fatalf("create provider key: %v", err)
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("create provider key: status %d: %s", resp.StatusCode, raw)
	}
	t.Logf("create provider key response: %s", strings.TrimSpace(string(raw)))

	// Sanity-check what bifrost stored.
	req, _ := http.NewRequest(http.MethodGet, bf.BaseURL+"/api/providers/openai/keys", nil)
	if r, err := bf.httpClient().Do(req); err == nil {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		t.Logf("verify provider keys (status=%d): %s", r.StatusCode, strings.TrimSpace(string(body)))
	}
}
