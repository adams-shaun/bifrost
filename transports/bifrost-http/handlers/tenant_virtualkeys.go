// Package handlers — tenant-scoped virtual-key admin.
//
// Mounts POST /api/tenants/{tenant_id}/governance/virtual-keys so a
// platform-admin can create a virtual key whose tenant_id is pinned
// explicitly (so the inference-plane VK->tenant resolver lookup returns
// the right tenant) without going through the legacy single-tenant
// governance VK endpoint.
//
// v1 minimum surface: POST only, simple payload. The legacy
// createVirtualKey at /api/governance/virtual-keys supports rich linkage
// (teams, customers, access profiles, rate limits, budgets,
// per-provider configs) — once tenant-admin auth lands in Phase 4 those
// features can be exposed under the tenant-scoped route family too.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// VKCacheInvalidator is the narrow contract the VK admin handlers need
// from the inference-plane resolver cache so a same-replica VK
// create/update/delete propagates immediately instead of waiting for
// the 60s TTL. Defined here so tests can stub it without spinning up
// a real CachedVKResolver.
//
// Cross-replica invalidation (Postgres LISTEN/NOTIFY or a versioned
// header from the admin API) is a separate follow-on; this only fixes
// the single-replica case.
type VKCacheInvalidator interface {
	Invalidate(vk string)
}

// VKGovernanceReloader is the slice of the GovernanceManager surface
// the tenant VK handler needs to reach into the governance plugin's
// in-memory store on create / delete.  Without this hook, a freshly-
// created tenant VK lives in the configstore but the governance
// plugin's request-path validator never sees it until process
// restart — every /v1 call against the new VK 401s with
// virtual_key_not_found.  ServerCallbacks (the legacy single-tenant
// handler's manager) satisfies this interface.
type VKGovernanceReloader interface {
	ReloadVirtualKey(ctx context.Context, id string) (*configstoreTables.TableVirtualKey, error)
	RemoveVirtualKey(ctx context.Context, id string) error
}

// TenantVirtualKeyHandler implements the tenant-scoped VK admin routes.
type TenantVirtualKeyHandler struct {
	configStore configstore.ConfigStore
	// evictor busts the cached per-tenant Bifrost runtime so the next
	// inference request lazy-loads with the new VK metadata in scope.
	// Optional.
	evictor TenantRuntimeEvictor
	// vkInvalidator drops the calling-replica's resolver cache entry
	// for the affected VK on create/update/delete. Optional — when
	// nil, the resolver TTL alone bounds staleness.
	vkInvalidator VKCacheInvalidator
	// governanceReloader hooks the governance plugin's in-memory VK
	// store so a freshly-created tenant VK is request-path-active
	// immediately, without a process restart. Optional — when nil,
	// new tenant VKs only become usable after the next governance-
	// store reload (typically at process restart).
	governanceReloader VKGovernanceReloader
}

// NewTenantVirtualKeyHandler constructs a TenantVirtualKeyHandler.
// configStore is required; all other args are optional and
// independently testable.
func NewTenantVirtualKeyHandler(configStore configstore.ConfigStore, evictor TenantRuntimeEvictor, vkInvalidator VKCacheInvalidator, governanceReloader VKGovernanceReloader) (*TenantVirtualKeyHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantVirtualKeyHandler{
		configStore:        configStore,
		evictor:            evictor,
		vkInvalidator:      vkInvalidator,
		governanceReloader: governanceReloader,
	}, nil
}

// RegisterRoutes mounts the tenant-scoped VK routes. middlewares MUST
// include handlers.RequireTenantPathMiddleware (or equivalent).
func (h *TenantVirtualKeyHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h.configStore == nil {
		return
	}
	r.GET("/api/tenants/{tenant_id}/governance/virtual-keys", lib.ChainMiddlewares(h.listVirtualKeys, middlewares...))
	r.POST("/api/tenants/{tenant_id}/governance/virtual-keys", lib.ChainMiddlewares(h.createVirtualKey, middlewares...))
	r.GET("/api/tenants/{tenant_id}/governance/virtual-keys/{vk_id}", lib.ChainMiddlewares(h.getVirtualKey, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/governance/virtual-keys/{vk_id}", lib.ChainMiddlewares(h.updateVirtualKey, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/governance/virtual-keys/{vk_id}", lib.ChainMiddlewares(h.deleteVirtualKey, middlewares...))
}

// UpdateTenantVirtualKeyRequest is the v1 minimum-viable PUT body. Only
// the safe-to-flip fields are accepted (name + description + active
// state); the VK value rotates via DELETE+POST rather than in-place
// update so callers can't accidentally invalidate live VK references.
type UpdateTenantVirtualKeyRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	IsActive    *bool   `json:"is_active,omitempty"`
}

// TenantVKProviderConfigRequest mirrors the per-provider config block
// from the legacy CreateVirtualKeyRequest, narrowed to the fields the
// tenant CRUD needs (provider name, weight, allowed_models). Provider
// config rate_limit / budgets / key_ids are not exposed here — they
// can be added when a use case requires it.
type TenantVKProviderConfigRequest struct {
	Provider      string            `json:"provider"`
	Weight        *float64          `json:"weight,omitempty"`
	AllowedModels schemas.WhiteList `json:"allowed_models,omitempty"`
}

// CreateTenantVirtualKeyRequest accepts the subset of the legacy
// CreateVirtualKeyRequest that the tenant golden-flow exercises: a
// top-level rate_limit + multi-budget set, and provider_configs that
// constrain which providers + models the VK can hit. MCP configs,
// access_profile_id, and team/customer linkage stay on the legacy
// single-tenant route; the same configstore methods power both so
// expanding here later is a struct change rather than handler logic.
type CreateTenantVirtualKeyRequest struct {
	Name string `json:"name"`
	// Value is optional. When empty, the server mints a fresh
	// governance-prefixed value so the caller doesn't have to know the
	// prefix. The response carries the resulting value so the E2E
	// harness (or the operator) can drive inference with it.
	Value string `json:"value,omitempty"`
	// Description is informational; safe to leave empty.
	Description string `json:"description,omitempty"`
	// IsActive defaults to true when omitted.
	IsActive        *bool                           `json:"is_active,omitempty"`
	CalendarAligned bool                            `json:"calendar_aligned,omitempty"`
	RateLimit       *CreateRateLimitRequest         `json:"rate_limit,omitempty"`
	Budgets         []CreateBudgetRequest           `json:"budgets,omitempty"`
	ProviderConfigs []TenantVKProviderConfigRequest `json:"provider_configs,omitempty"`
}

// CreateTenantVirtualKeyResponse is what we return on success — the
// generated id, the (possibly server-minted) value, and a readback of
// the supplied/derived fields.
type CreateTenantVirtualKeyResponse struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Value    string `json:"value"`
}

func (h *TenantVirtualKeyHandler) createVirtualKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}

	var req CreateTenantVirtualKeyRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "name is required")
		return
	}

	// Top-level budget validation: same shape as legacy createVirtualKey
	// — must have unique reset_duration values across the multi-budget set.
	if len(req.Budgets) > 0 {
		seen := make(map[string]bool, len(req.Budgets))
		for _, b := range req.Budgets {
			if b.MaxLimit < 0 {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Budget max_limit cannot be negative: %.2f", b.MaxLimit))
				return
			}
			if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid reset duration format: %s", b.ResetDuration))
				return
			}
			if seen[b.ResetDuration] {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Duplicate reset_duration in budgets: %s", b.ResetDuration))
				return
			}
			seen[b.ResetDuration] = true
		}
	}

	// Provider validation: every provider in provider_configs must
	// already be configured for this tenant. Empty provider_configs
	// means "no providers allowed" (deny-by-default) — the legacy
	// handler's semantics — but for the tenant route we currently
	// have no MCP/team linkage so an empty list is the only signal.
	var tenantProviderSet map[schemas.ModelProvider]struct{}
	if len(req.ProviderConfigs) > 0 {
		var err error
		tenantProviderSet, err = h.getTenantProviderSet(ctx, string(tid))
		if err != nil {
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load tenant providers: %v", err))
			return
		}
	}

	value := req.Value
	if value == "" {
		value = governance.GenerateVirtualKey()
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	vk := &configstoreTables.TableVirtualKey{
		ID:              uuid.NewString(),
		TenantID:        string(tid),
		Name:            req.Name,
		Value:           value,
		Description:     req.Description,
		IsActive:        &isActive,
		CalendarAligned: req.CalendarAligned,
	}

	// Minimal POST (no rate_limit / budgets / provider_configs) takes the
	// direct CreateVirtualKey path so existing happy-path tests (with
	// stub stores that don't implement ExecuteTransaction) keep working.
	if req.RateLimit == nil && len(req.Budgets) == 0 && len(req.ProviderConfigs) == 0 {
		if err := h.configStore.CreateVirtualKey(ctx, vk); err != nil {
			if errors.Is(err, configstore.ErrAlreadyExists) {
				SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Virtual key %s already exists for tenant %s", req.Name, tid))
				return
			}
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create virtual key: %v", err))
			return
		}
		h.postCreateInvalidate(tid, vk.Value)
		h.reloadGovernanceVK(ctx, vk.ID)
		SendJSONWithStatus(ctx, map[string]any{
			"message":     "Virtual key created successfully",
			"virtual_key": vk,
		}, fasthttp.StatusCreated)
		return
	}

	// Rich POST: VK + owned rate_limit / budgets / provider_configs run
	// inside a single transaction so a half-built VK can't leak when a
	// later step fails.
	if err := h.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// VK-level rate limit
		if req.RateLimit != nil {
			rl := &configstoreTables.TableRateLimit{
				ID:                   uuid.NewString(),
				TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
				TokenResetDuration:   req.RateLimit.TokenResetDuration,
				RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
				RequestResetDuration: req.RateLimit.RequestResetDuration,
				TokenLastReset:       time.Now(),
				RequestLastReset:     time.Now(),
			}
			if err := h.configStore.CreateRateLimitForTenant(ctx, string(tid), rl, tx); err != nil {
				return err
			}
			vk.RateLimitID = &rl.ID
		}

		if err := h.configStore.CreateVirtualKey(ctx, vk, tx); err != nil {
			return err
		}

		// VK-level multi-budget
		for _, b := range req.Budgets {
			budget := &configstoreTables.TableBudget{
				ID:            uuid.NewString(),
				MaxLimit:      b.MaxLimit,
				ResetDuration: b.ResetDuration,
				LastReset:     budgetLastReset(vk.CalendarAligned, b.ResetDuration),
				CurrentUsage:  0,
				VirtualKeyID:  &vk.ID,
			}
			if err := h.configStore.CreateBudgetForTenant(ctx, string(tid), budget, tx); err != nil {
				return err
			}
		}

		// Provider configs: pin which providers + models this VK may use.
		for _, pc := range req.ProviderConfigs {
			providerName := schemas.ModelProvider(strings.TrimSpace(pc.Provider))
			if providerName == "" {
				return fmt.Errorf("provider name is required in provider_configs entry")
			}
			if _, configured := tenantProviderSet[providerName]; !configured {
				return fmt.Errorf("provider %s is not configured for tenant %s", pc.Provider, tid)
			}
			if err := pc.AllowedModels.Validate(); err != nil {
				return fmt.Errorf("invalid allowed_models for provider %s: %w", pc.Provider, err)
			}
			pcRow := &configstoreTables.TableVirtualKeyProviderConfig{
				VirtualKeyID:  vk.ID,
				Provider:      string(providerName),
				Weight:        pc.Weight,
				AllowedModels: pc.AllowedModels,
				// Tenant route doesn't expose key_ids — default to "allow all
				// keys for the provider" so requests through this VK can use
				// whatever keys the tenant has configured for the provider.
				AllowAllKeys: true,
			}
			if err := h.configStore.CreateVirtualKeyProviderConfig(ctx, pcRow, tx); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Virtual key %s already exists for tenant %s", req.Name, tid))
			return
		}
		// Provider-validation and budget-validation errors are user input
		// errors — surface as 400 instead of 500.
		msg := err.Error()
		if strings.Contains(msg, "is not configured for tenant") || strings.Contains(msg, "invalid allowed_models") || strings.Contains(msg, "provider name is required") {
			SendError(ctx, fasthttp.StatusBadRequest, msg)
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create virtual key: %v", err))
		return
	}

	// Best-effort cache invalidation. The VK resolver caches lookups for
	// 60s; an existing inference request mid-flight already has a
	// resolved tenant on its ctx, so this matters most for *subsequent*
	// requests with the freshly minted VK. Evicting the tenant runtime
	// (if loaded) keeps the per-tenant Account in sync if downstream
	// patches start coupling VK metadata into the Account.
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	if h.vkInvalidator != nil {
		// Drop any stale cached entry for this VK value so the very
		// next inference request sees the freshly-minted mapping.
		// (Usually no entry exists yet on create; this is the path
		// where a caller supplied an explicit Value that happened to
		// match a previously-resolved-but-then-deleted-and-recreated
		// VK.)
		h.vkInvalidator.Invalidate(vk.Value)
	}
	h.reloadGovernanceVK(ctx, vk.ID)

	// Return the legacy {message, virtual_key} envelope so the UI's
	// createVirtualKey mutation (typed as {message: string; virtual_key:
	// VirtualKey}) round-trips. The optimistic-update onQueryStarted
	// reads data.virtual_key.id / .name to splice into the cached list.
	SendJSONWithStatus(ctx, map[string]any{
		"message":     "Virtual key created successfully",
		"virtual_key": vk,
	}, fasthttp.StatusCreated)
}

// reloadGovernanceVK pokes the governance plugin's in-memory store so a
// freshly-created tenant VK is request-path-active immediately. Best-
// effort: when the reloader isn't wired (tests, single-tenant deploys)
// or the reload fails (db lag, etc.) we keep going; the resolver TTL +
// the next governance restart still close the loop, just slower.
func (h *TenantVirtualKeyHandler) reloadGovernanceVK(ctx context.Context, vkID string) {
	if h.governanceReloader == nil {
		return
	}
	if _, err := h.governanceReloader.ReloadVirtualKey(ctx, vkID); err != nil {
		// Don't fail the request — the VK is persisted; this is a
		// cache nudge. Log via the configstore-side logger if/when
		// the handler gets one, but the silent best-effort path
		// matches the existing evictor / vkInvalidator pattern.
		_ = err
	}
}

// removeGovernanceVK is the delete-path counterpart of reloadGovernanceVK.
func (h *TenantVirtualKeyHandler) removeGovernanceVK(ctx context.Context, vkID string) {
	if h.governanceReloader == nil {
		return
	}
	if err := h.governanceReloader.RemoveVirtualKey(ctx, vkID); err != nil {
		_ = err
	}
}

// postCreateInvalidate centralises the runtime evictor + resolver-
// cache invalidator calls that both the minimal and rich VK create
// paths need to make.
func (h *TenantVirtualKeyHandler) postCreateInvalidate(tenantID multitenant.TenantID, vkValue string) {
	if h.evictor != nil {
		h.evictor.Evict(tenantID)
	}
	if h.vkInvalidator != nil {
		h.vkInvalidator.Invalidate(vkValue)
	}
}

// getTenantProviderSet returns the set of providers configured for a
// tenant, used to validate VK provider_configs entries. Mirrors the
// global getConfiguredProviderSet helper in governance.go but scopes
// the lookup to a single tenant.
func (h *TenantVirtualKeyHandler) getTenantProviderSet(ctx *fasthttp.RequestCtx, tenantID string) (map[schemas.ModelProvider]struct{}, error) {
	cfgs, err := h.configStore.GetProvidersConfigByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	set := make(map[schemas.ModelProvider]struct{}, len(cfgs))
	for p := range cfgs {
		set[p] = struct{}{}
	}
	return set, nil
}

// listVirtualKeys — GET /api/tenants/{tenant_id}/governance/virtual-keys
func (h *TenantVirtualKeyHandler) listVirtualKeys(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	vks, err := h.configStore.GetVirtualKeysByTenant(ctx, string(tid))
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list virtual keys: %v", err))
		return
	}
	// Match the legacy GetVirtualKeysResponse shape — UI's getVirtualKeys
	// query reads response.virtual_keys + response.count + total_count +
	// limit + offset. The {tenant_id, total} envelope used to render
	// zero rows because draft.virtual_keys was undefined at the type
	// level.
	SendJSON(ctx, map[string]any{
		"virtual_keys": vks,
		"count":        len(vks),
		"total_count":  len(vks),
		"limit":        len(vks),
		"offset":       0,
	})
}

// getVirtualKey — GET /api/tenants/{tenant_id}/governance/virtual-keys/{vk_id}
func (h *TenantVirtualKeyHandler) getVirtualKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	vkID, _ := ctx.UserValue("vk_id").(string)
	if vkID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "vk_id is required")
		return
	}
	vk, err := h.configStore.GetVirtualKeyByIDForTenant(ctx, string(tid), vkID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Virtual key %s not found for tenant %s", vkID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get virtual key: %v", err))
		return
	}
	// UI's getVirtualKey query is typed as {virtual_key: VirtualKey};
	// matches the legacy /api/governance/virtual-keys/{id} shape.
	SendJSON(ctx, map[string]any{"virtual_key": vk})
}

// updateVirtualKey — PUT /api/tenants/{tenant_id}/governance/virtual-keys/{vk_id}
//
// Accepts the safe-to-flip fields (name, description, is_active). Only
// fields explicitly present in the body are touched; omitted fields
// keep their current value. Evicts the per-tenant runtime so cached
// VK metadata is dropped.
func (h *TenantVirtualKeyHandler) updateVirtualKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	vkID, _ := ctx.UserValue("vk_id").(string)
	if vkID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "vk_id is required")
		return
	}

	var req UpdateTenantVirtualKeyRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Name == nil && req.Description == nil && req.IsActive == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "at least one of name, description, is_active is required")
		return
	}

	existing, err := h.configStore.GetVirtualKeyByIDForTenant(ctx, string(tid), vkID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Virtual key %s not found for tenant %s", vkID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load virtual key: %v", err))
		return
	}

	// Apply the patch in place. UpdateVirtualKeyForTenant re-pins
	// tenant_id on the row before delegating, so a malicious payload
	// can't reach this far.
	if req.Name != nil {
		existing.Name = *req.Name
	}
	if req.Description != nil {
		existing.Description = *req.Description
	}
	if req.IsActive != nil {
		existing.IsActive = req.IsActive
	}

	if err := h.configStore.UpdateVirtualKeyForTenant(ctx, string(tid), existing); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Virtual key %s not found for tenant %s", vkID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update virtual key: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	if h.vkInvalidator != nil && existing.Value != "" {
		// PUT can flip IsActive (turn a VK off). Inference checks
		// IsActive, but the resolver cache returned the tenant_id
		// before the check fires — so a stale cache hit with
		// IsActive=true was still bouncing requests to the runtime
		// for up to 60s after the admin "deactivated" the VK. Drop
		// the entry so the next ResolveVK rereads and surfaces the
		// new IsActive value.
		h.vkInvalidator.Invalidate(existing.Value)
	}
	// UI's updateVirtualKey mutation is typed as
	// {message: string; virtual_key: VirtualKey}.
	SendJSON(ctx, map[string]any{
		"message":     "Virtual key updated successfully",
		"virtual_key": existing,
	})
}

// deleteVirtualKey — DELETE /api/tenants/{tenant_id}/governance/virtual-keys/{vk_id}
//
// 204 on success, 404 on cross-tenant / missing. Evicts the per-tenant
// runtime so any cached resolver state for the removed VK is dropped.
func (h *TenantVirtualKeyHandler) deleteVirtualKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	vkID, _ := ctx.UserValue("vk_id").(string)
	if vkID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "vk_id is required")
		return
	}
	// Capture the VK value before delete so we can invalidate the
	// resolver cache after the row is gone. If the lookup itself
	// 404s we just skip the invalidation and let the DELETE bubble
	// the same 404 through to the caller.
	var deletedValue string
	if h.vkInvalidator != nil {
		if vk, lookupErr := h.configStore.GetVirtualKeyByIDForTenant(ctx, string(tid), vkID); lookupErr == nil && vk != nil {
			deletedValue = vk.Value
		}
	}
	if err := h.configStore.DeleteVirtualKeyForTenant(ctx, string(tid), vkID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Virtual key %s not found for tenant %s", vkID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete virtual key: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	if h.vkInvalidator != nil && deletedValue != "" {
		h.vkInvalidator.Invalidate(deletedValue)
	}
	h.removeGovernanceVK(ctx, vkID)
	// UI's deleteVirtualKey mutation is typed as {message: string};
	// matches the legacy /api/governance/virtual-keys/{id} DELETE.
	SendJSON(ctx, map[string]any{"message": "Virtual key deleted successfully"})
}
