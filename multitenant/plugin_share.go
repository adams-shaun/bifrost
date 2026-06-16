// Package multitenant — plugin_share.go.
//
// Why this exists: the tenant loader (see transports/bifrost-http/server/
// server.go) hands the SAME LLMPlugin / MCPPlugin instances the root
// bifrost.Bifrost was built with to every per-tenant bifrost.Bifrost
// instance.  That's what makes a /v1 request routed through a tenant
// runtime still run the logging, governance, telemetry plugins.
//
// But bifrost.Bifrost.Shutdown() calls plugin.Cleanup() on every plugin
// it owns.  When the Manager evicts a tenant runtime (a key/VK admin
// write triggers Evict; LRU pressure triggers Evict; Shutdown evicts
// all), the evicted tenant Bifrost.Shutdown() runs Cleanup() on the
// shared plugins — which puts them in a torn-down state for everyone,
// including the still-live root runtime.  Symptom: after a single
// tenant eviction nothing logs anymore (root or tenant), because the
// logging plugin's writer is closed.
//
// The fix here keeps the upside (one plugin chain, shared across all
// runtimes) without the downside: wrap each plugin in a forwarder that
// passes every hook through to the underlying instance but turns
// Cleanup() into a no-op.  The root runtime still owns the original
// plugins and runs the real Cleanup at process shutdown.

package multitenant

import "github.com/maximhq/bifrost/core/schemas"

// sharedLLMPlugin forwards every LLMPlugin call to the wrapped plugin
// but skips Cleanup so per-tenant Shutdown can't break shared state.
type sharedLLMPlugin struct {
	inner schemas.LLMPlugin
}

func (s *sharedLLMPlugin) GetName() string { return s.inner.GetName() }
func (s *sharedLLMPlugin) Cleanup() error  { return nil }
func (s *sharedLLMPlugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return s.inner.PreLLMHook(ctx, req)
}
func (s *sharedLLMPlugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return s.inner.PostLLMHook(ctx, resp, bifrostErr)
}

// sharedMCPPlugin is the MCPPlugin counterpart of sharedLLMPlugin.
type sharedMCPPlugin struct {
	inner schemas.MCPPlugin
}

func (s *sharedMCPPlugin) GetName() string { return s.inner.GetName() }
func (s *sharedMCPPlugin) Cleanup() error  { return nil }
func (s *sharedMCPPlugin) PreMCPHook(ctx *schemas.BifrostContext, req *schemas.BifrostMCPRequest) (*schemas.BifrostMCPRequest, *schemas.MCPPluginShortCircuit, error) {
	return s.inner.PreMCPHook(ctx, req)
}
func (s *sharedMCPPlugin) PostMCPHook(ctx *schemas.BifrostContext, resp *schemas.BifrostMCPResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostMCPResponse, *schemas.BifrostError, error) {
	return s.inner.PostMCPHook(ctx, resp, bifrostErr)
}

// ShareLLMPlugins returns a slice of LLMPlugin wrappers safe to pass to
// a per-tenant bifrost.Bifrost.  Each wrapper forwards all hooks to the
// original instance but ignores Cleanup, so the tenant runtime's
// Shutdown can't tear down state the root runtime still depends on.
func ShareLLMPlugins(plugins []schemas.LLMPlugin) []schemas.LLMPlugin {
	if len(plugins) == 0 {
		return nil
	}
	out := make([]schemas.LLMPlugin, len(plugins))
	for i, p := range plugins {
		out[i] = &sharedLLMPlugin{inner: p}
	}
	return out
}

// ShareMCPPlugins is the MCPPlugin counterpart of ShareLLMPlugins.
func ShareMCPPlugins(plugins []schemas.MCPPlugin) []schemas.MCPPlugin {
	if len(plugins) == 0 {
		return nil
	}
	out := make([]schemas.MCPPlugin, len(plugins))
	for i, p := range plugins {
		out[i] = &sharedMCPPlugin{inner: p}
	}
	return out
}
