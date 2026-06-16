// Package handlers — tenant-scoped governance budgets + rate limits.
//
// Mounts CRUD for the two remaining governance metadata entities under
// /api/tenants/{tenant_id}/governance/{budgets,rate-limits}. Like
// teams + customers, these are configuration data attached to other
// entities (VKs / providers / teams) rather than request-path state,
// so the runtime evictor is intentionally not wired here.
package handlers

import (
	"errors"
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// ---- Budgets ----

type TenantBudgetsHandler struct {
	configStore configstore.ConfigStore
}

func NewTenantBudgetsHandler(configStore configstore.ConfigStore) (*TenantBudgetsHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantBudgetsHandler{configStore: configStore}, nil
}

func (h *TenantBudgetsHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h.configStore == nil {
		return
	}
	r.GET("/api/tenants/{tenant_id}/governance/budgets", lib.ChainMiddlewares(h.list, middlewares...))
	r.POST("/api/tenants/{tenant_id}/governance/budgets", lib.ChainMiddlewares(h.create, middlewares...))
	r.GET("/api/tenants/{tenant_id}/governance/budgets/{budget_id}", lib.ChainMiddlewares(h.get, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/governance/budgets/{budget_id}", lib.ChainMiddlewares(h.update, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/governance/budgets/{budget_id}", lib.ChainMiddlewares(h.delete, middlewares...))
}

// CreateTenantBudgetRequest is the minimum-viable POST body.
type CreateTenantBudgetRequest struct {
	ID            string  `json:"id,omitempty"`
	MaxLimit      float64 `json:"max_limit"`
	ResetDuration string  `json:"reset_duration"`
}

// UpdateTenantBudgetRequest accepts the safe-to-flip fields.
type UpdateTenantBudgetRequest struct {
	MaxLimit      *float64 `json:"max_limit,omitempty"`
	ResetDuration *string  `json:"reset_duration,omitempty"`
}

func (h *TenantBudgetsHandler) list(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	budgets, err := h.configStore.GetBudgetsByTenant(ctx, string(tid))
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list budgets: %v", err))
		return
	}
	// Match GetBudgetsResponse — {budgets, count, total_count, limit, offset}.
	SendJSON(ctx, map[string]any{
		"budgets":     budgets,
		"count":       len(budgets),
		"total_count": len(budgets),
		"limit":       len(budgets),
		"offset":      0,
	})
}

func (h *TenantBudgetsHandler) create(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	var req CreateTenantBudgetRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.MaxLimit < 0 {
		SendError(ctx, fasthttp.StatusBadRequest, "max_limit cannot be negative")
		return
	}
	if req.ResetDuration == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "reset_duration is required")
		return
	}
	if _, err := configstoreTables.ParseDuration(req.ResetDuration); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid reset_duration: %v", err))
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	b := &configstoreTables.TableBudget{ID: req.ID, MaxLimit: req.MaxLimit, ResetDuration: req.ResetDuration}
	if err := h.configStore.CreateBudgetForTenant(ctx, string(tid), b); err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create budget: %v", err))
		return
	}
	SendJSONWithStatus(ctx, map[string]any{
		"message": "Budget created successfully",
		"budget":  b,
	}, fasthttp.StatusCreated)
}

func (h *TenantBudgetsHandler) get(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	id, _ := ctx.UserValue("budget_id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "budget_id is required")
		return
	}
	b, err := h.configStore.GetBudgetForTenant(ctx, string(tid), id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Budget %s not found for tenant %s", id, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get budget: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"budget": b})
}

func (h *TenantBudgetsHandler) update(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	id, _ := ctx.UserValue("budget_id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "budget_id is required")
		return
	}
	var req UpdateTenantBudgetRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.MaxLimit == nil && req.ResetDuration == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "at least one of max_limit, reset_duration is required")
		return
	}
	existing, err := h.configStore.GetBudgetForTenant(ctx, string(tid), id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Budget %s not found for tenant %s", id, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load budget: %v", err))
		return
	}
	if req.MaxLimit != nil {
		if *req.MaxLimit < 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "max_limit cannot be negative")
			return
		}
		existing.MaxLimit = *req.MaxLimit
	}
	if req.ResetDuration != nil {
		if _, derr := configstoreTables.ParseDuration(*req.ResetDuration); derr != nil {
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid reset_duration: %v", derr))
			return
		}
		existing.ResetDuration = *req.ResetDuration
	}
	if err := h.configStore.UpdateBudgetForTenant(ctx, string(tid), existing); err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update budget: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{
		"message": "Budget updated successfully",
		"budget":  existing,
	})
}

func (h *TenantBudgetsHandler) delete(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	id, _ := ctx.UserValue("budget_id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "budget_id is required")
		return
	}
	if err := h.configStore.DeleteBudgetForTenant(ctx, string(tid), id); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Budget %s not found for tenant %s", id, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete budget: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"message": "Budget deleted successfully"})
}

// ---- Rate limits ----

type TenantRateLimitsHandler struct {
	configStore configstore.ConfigStore
}

func NewTenantRateLimitsHandler(configStore configstore.ConfigStore) (*TenantRateLimitsHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantRateLimitsHandler{configStore: configStore}, nil
}

func (h *TenantRateLimitsHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h.configStore == nil {
		return
	}
	r.GET("/api/tenants/{tenant_id}/governance/rate-limits", lib.ChainMiddlewares(h.list, middlewares...))
	r.POST("/api/tenants/{tenant_id}/governance/rate-limits", lib.ChainMiddlewares(h.create, middlewares...))
	r.GET("/api/tenants/{tenant_id}/governance/rate-limits/{rate_limit_id}", lib.ChainMiddlewares(h.get, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/governance/rate-limits/{rate_limit_id}", lib.ChainMiddlewares(h.update, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/governance/rate-limits/{rate_limit_id}", lib.ChainMiddlewares(h.delete, middlewares...))
}

// CreateTenantRateLimitRequest is the minimum-viable POST body. Both
// pairs (token / request) are optional but at least one must be set.
type CreateTenantRateLimitRequest struct {
	ID                   string  `json:"id,omitempty"`
	TokenMaxLimit        *int64  `json:"token_max_limit,omitempty"`
	TokenResetDuration   *string `json:"token_reset_duration,omitempty"`
	RequestMaxLimit      *int64  `json:"request_max_limit,omitempty"`
	RequestResetDuration *string `json:"request_reset_duration,omitempty"`
}

type UpdateTenantRateLimitRequest = CreateTenantRateLimitRequest

func (h *TenantRateLimitsHandler) list(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	rls, err := h.configStore.GetRateLimitsByTenant(ctx, string(tid))
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list rate limits: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{
		"rate_limits": rls,
		"count":       len(rls),
		"total_count": len(rls),
		"limit":       len(rls),
		"offset":      0,
	})
}

func (h *TenantRateLimitsHandler) create(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	var req CreateTenantRateLimitRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.TokenMaxLimit == nil && req.RequestMaxLimit == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "at least one of token_max_limit or request_max_limit is required")
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	rl := &configstoreTables.TableRateLimit{
		ID:                   req.ID,
		TokenMaxLimit:        req.TokenMaxLimit,
		TokenResetDuration:   req.TokenResetDuration,
		RequestMaxLimit:      req.RequestMaxLimit,
		RequestResetDuration: req.RequestResetDuration,
	}
	if err := h.configStore.CreateRateLimitForTenant(ctx, string(tid), rl); err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create rate limit: %v", err))
		return
	}
	SendJSONWithStatus(ctx, map[string]any{
		"message":    "Rate limit created successfully",
		"rate_limit": rl,
	}, fasthttp.StatusCreated)
}

func (h *TenantRateLimitsHandler) get(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	id, _ := ctx.UserValue("rate_limit_id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "rate_limit_id is required")
		return
	}
	rl, err := h.configStore.GetRateLimitForTenant(ctx, string(tid), id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Rate limit %s not found for tenant %s", id, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get rate limit: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"rate_limit": rl})
}

func (h *TenantRateLimitsHandler) update(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	id, _ := ctx.UserValue("rate_limit_id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "rate_limit_id is required")
		return
	}
	var req UpdateTenantRateLimitRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.TokenMaxLimit == nil && req.RequestMaxLimit == nil && req.TokenResetDuration == nil && req.RequestResetDuration == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "at least one field is required")
		return
	}
	existing, err := h.configStore.GetRateLimitForTenant(ctx, string(tid), id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Rate limit %s not found for tenant %s", id, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load rate limit: %v", err))
		return
	}
	if req.TokenMaxLimit != nil {
		existing.TokenMaxLimit = req.TokenMaxLimit
	}
	if req.TokenResetDuration != nil {
		existing.TokenResetDuration = req.TokenResetDuration
	}
	if req.RequestMaxLimit != nil {
		existing.RequestMaxLimit = req.RequestMaxLimit
	}
	if req.RequestResetDuration != nil {
		existing.RequestResetDuration = req.RequestResetDuration
	}
	if err := h.configStore.UpdateRateLimitForTenant(ctx, string(tid), existing); err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update rate limit: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{
		"message":    "Rate limit updated successfully",
		"rate_limit": existing,
	})
}

func (h *TenantRateLimitsHandler) delete(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx")
		return
	}
	id, _ := ctx.UserValue("rate_limit_id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "rate_limit_id is required")
		return
	}
	if err := h.configStore.DeleteRateLimitForTenant(ctx, string(tid), id); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Rate limit %s not found for tenant %s", id, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete rate limit: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"message": "Rate limit deleted successfully"})
}
