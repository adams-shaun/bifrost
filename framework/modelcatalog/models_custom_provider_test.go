// f5xc-overlay (patch17): tests for the bare-ID handling on custom
// providers in UpsertModelDataForProvider /
// UpsertUnfilteredModelDataForProvider.
//
// Truth table the tests lock in:
//
//   model.ID shape          isCustom=false (upstream)  isCustom=true (f5xc)
//   "openai/gpt-4o"         accepted under "openai"    accepted under "openai"
//   "qwen3.6-coder" (bare)  DROPPED                    accepted under provider
//   "qwen/qwen3.6-coder"    accepted under "qwen"      accepted under "qwen"
//
// The "isCustom=false drops bare IDs" row is the upstream behaviour we
// MUST preserve — calling code that doesn't know to set the flag (or
// is wired against the upstream-pinned framework module via the SDK
// repo) sees identical semantics.

package modelcatalog

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestUpsertModelDataForProvider_CustomProvider_AcceptsBareID(t *testing.T) {
	mc := NewTestCatalog(nil)
	provider := schemas.ModelProvider("qwen")

	mc.UpsertModelDataForProvider(provider, &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "qwen3.6-coder"}, // vLLM-style bare id
			{ID: "qwen3.6-coder-instruct"},
		},
	}, nil, true /* isCustom */)

	got := mc.GetModelsForProvider(provider)
	want := map[string]bool{"qwen3.6-coder": true, "qwen3.6-coder-instruct": true}
	if len(got) != len(want) {
		t.Fatalf("want %d models, got %d (%v)", len(want), len(got), got)
	}
	for _, m := range got {
		if !want[m] {
			t.Fatalf("unexpected model in catalog: %q (want %v)", m, want)
		}
	}
}

func TestUpsertModelDataForProvider_NonCustomProvider_DropsBareID(t *testing.T) {
	// Regression guard: when isCustom=false (upstream call path), bare
	// IDs MUST still be filtered out. Otherwise we'd change upstream
	// semantics for built-in providers.
	mc := NewTestCatalog(nil)
	provider := schemas.ModelProvider("openai")

	mc.UpsertModelDataForProvider(provider, &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "should-be-dropped-because-no-prefix"},
			{ID: "openai/gpt-4o"},
		},
	}, nil, false /* isCustom — upstream caller */)

	got := mc.GetModelsForProvider(provider)
	if len(got) != 1 || got[0] != "gpt-4o" {
		t.Fatalf("want exactly [gpt-4o], got %v", got)
	}
}

func TestUpsertModelDataForProvider_CustomProvider_PrefixedIDStillWorks(t *testing.T) {
	// If a custom backend happens to return prefixed IDs (some proxies
	// do), the isCustom branch should not regress that path.
	//
	// Important: in production, schemas.RegisterKnownProvider("qwen") is
	// called by core/bifrost.go::prepareProvider when the custom
	// provider is added (upstream commit 60072820f7), so ParseModelString
	// learns to split on "qwen/". We mirror that here so the test
	// exercises the same path real traffic does.
	schemas.RegisterKnownProvider("qwen")
	defer schemas.UnregisterKnownProvider("qwen")

	mc := NewTestCatalog(nil)
	provider := schemas.ModelProvider("qwen")

	mc.UpsertModelDataForProvider(provider, &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "qwen/qwen3.6-coder"},
		},
	}, nil, true)

	got := mc.GetModelsForProvider(provider)
	if len(got) != 1 || got[0] != "qwen3.6-coder" {
		t.Fatalf("want [qwen3.6-coder], got %v", got)
	}
}

func TestUpsertUnfilteredModelDataForProvider_CustomProvider_AcceptsBareID(t *testing.T) {
	// Same fix on the unfiltered path — covered separately because
	// the unfiltered upsert is a sibling function that previously had
	// the identical filter bug.
	mc := NewTestCatalog(nil)
	provider := schemas.ModelProvider("qwen")

	mc.UpsertUnfilteredModelDataForProvider(provider, &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "qwen3.6-coder"},
		},
	}, true /* isCustom */)

	got := mc.unfilteredModelPool[provider]
	if len(got) != 1 || got[0] != "qwen3.6-coder" {
		t.Fatalf("unfiltered: want [qwen3.6-coder], got %v", got)
	}
}

func TestGetProvidersForModel_AfterCustomProviderUpsert(t *testing.T) {
	// End-to-end of the bug-2 chain: bare ID arrives via discovery,
	// gets accepted under the custom provider, and GetProvidersForModel
	// returns the provider when queried with the bare name. This is the
	// path that resolveModelAndProvider walks on /v1/chat/completions,
	// so this test proves auto-resolve works for custom providers once
	// the catalog has been populated.
	mc := NewTestCatalog(nil)
	provider := schemas.ModelProvider("qwen")

	mc.UpsertModelDataForProvider(provider, &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "qwen3.6-coder"},
		},
	}, nil, true)

	got := mc.GetProvidersForModel("qwen3.6-coder")
	if len(got) != 1 || got[0] != provider {
		t.Fatalf("auto-resolve: want [qwen], got %v", got)
	}
}
