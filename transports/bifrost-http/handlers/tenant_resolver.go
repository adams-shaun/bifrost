package handlers

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

// TenantResolverMiddleware extracts the inbound virtual key from the request,
// resolves it to a tenant id via the supplied multitenant.VKResolver, and
// stashes the result on the fasthttp RequestCtx under
// multitenant.BifrostContextKeyTenantID. Downstream BifrostContext
// construction (lib/ctx.go) lifts that user-value onto the BifrostContext,
// where the per-tenant Bifrost dispatcher reads it.
//
// Behaviour:
//
//   - VK header missing: pass through unchanged. The downstream pipeline
//     still has the option to attribute the request to the default tenant.
//   - VK present but unknown / inactive: still pass through; we DO NOT reject
//     here so the existing governance plugin keeps owning auth/authz errors
//     with a single source of truth. The tenant id is simply not set.
//   - Resolver error (DB outage, etc.): logged at debug level, request still
//     passes through without a tenant id. Failing closed at this layer would
//     translate every transient DB blip into 500s for inference; the
//     dispatcher can decide its own fail-open / fail-closed policy.
//
// Skip paths: requests under /api/platform/* never carry a tenant context
// (platform-admin APIs are cross-tenant); /health is too noisy to look up.
//
// The middleware reads the same VK header shapes as lib/ctx.go's
// BifrostContext construction (x-bf-vk, Authorization: Bearer sk-bf-...,
// x-api-key, x-goog-api-key) so behaviour stays aligned with the existing
// governance plugin's VK source of truth.
func TenantResolverMiddleware(resolver multitenant.VKResolver) schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			if resolver == nil {
				next(ctx)
				return
			}
			path := string(ctx.RequestURI())
			if shouldSkipTenantResolve(path) {
				next(ctx)
				return
			}

			vk := extractVKFromHeaders(&ctx.Request.Header)
			if vk == "" {
				next(ctx)
				return
			}

			tid, err := resolver.ResolveVK(ctx, vk)
			if err != nil {
				// Unknown / inactive VK / DB error all fall through without a
				// tenant id. Governance still owns the rejection path.
				if logger != nil {
					logger.Debug("tenant resolver: VK lookup failed (vk=%q err=%v) — request continues without tenant_id", redactVK(vk), err)
				}
				next(ctx)
				return
			}

			ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), string(tid))
			next(ctx)
		}
	}
}

// shouldSkipTenantResolve returns true for paths that have no per-tenant
// dispatch and so don't benefit from a VK lookup. Keep this list short —
// every entry is a per-request VK lookup we avoid.
func shouldSkipTenantResolve(path string) bool {
	if path == "/health" {
		return true
	}
	if strings.HasPrefix(path, "/api/platform/") {
		return true
	}
	return false
}

// extractVKFromHeaders mirrors the VK ingestion logic in lib/ctx.go so the
// resolver sees the same VK the governance plugin will later see. It only
// returns a non-empty value when the candidate clearly looks like a virtual
// key (x-bf-vk is the dedicated header; the other shapes are accepted only
// when the value starts with the governance VK prefix).
func extractVKFromHeaders(h *fasthttp.RequestHeader) string {
	if v := string(h.Peek("x-bf-vk")); v != "" {
		return v
	}
	if v := string(h.Peek("Authorization")); v != "" {
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			cand := strings.TrimSpace(v[7:])
			if strings.HasPrefix(strings.ToLower(cand), virtualKeyPrefix) {
				return cand
			}
		}
	}
	if v := string(h.Peek("x-api-key")); v != "" && strings.HasPrefix(strings.ToLower(v), virtualKeyPrefix) {
		return v
	}
	if v := string(h.Peek("x-goog-api-key")); v != "" && strings.HasPrefix(strings.ToLower(v), virtualKeyPrefix) {
		return v
	}
	return ""
}

// virtualKeyPrefix duplicates governance.VirtualKeyPrefix so this file does
// not pull in the governance plugin import for a single constant. Keep in
// sync; the value is governance-spec-stable.
const virtualKeyPrefix = "sk-bf-"

// redactVK trims a VK down to a stable prefix + last 4 chars for logging.
func redactVK(vk string) string {
	if len(vk) <= 8 {
		return "redacted"
	}
	return vk[:6] + "…" + vk[len(vk)-4:]
}
