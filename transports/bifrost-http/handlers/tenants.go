// Package handlers — platform-admin tenant CRUD.
//
// This file lives separate from governance.go because tenants are above the
// governance hierarchy, not inside it. Routes are mounted under
// /api/platform/tenants so the access-control layer can apply
// platform-admin-only auth (cross-tenant) rather than the per-tenant scope
// that governance endpoints use.
package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TenantsHandler exposes platform-admin CRUD for the tenant entity.
// Tenants are the top-level isolation boundary for multi-tenant deployments;
// every governance and user-config row carries a tenant_id, and the per-tenant
// Bifrost runtime registry uses that to dispatch into the right tenant's world.
type TenantsHandler struct {
	configStore configstore.ConfigStore
}

// NewTenantsHandler constructs a TenantsHandler. Returns an error if the
// ConfigStore is nil — tenants must be persisted.
func NewTenantsHandler(configStore configstore.ConfigStore) (*TenantsHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantsHandler{configStore: configStore}, nil
}

// CreateTenantRequest is the POST /api/platform/tenants body.
//
// ID is required (the platform admin picks a stable slug-like identifier).
// Auto-generation would prevent IdP and SCIM integrations from minting
// tenants with stable external IDs.
type CreateTenantRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
}

// UpdateTenantRequest is the PUT /api/platform/tenants/{tenant_id} body.
// Only the mutable fields are accepted; ID cannot change.
type UpdateTenantRequest struct {
	Name        *string `json:"name,omitempty"`
	Status      *string `json:"status,omitempty"`
	Description *string `json:"description,omitempty"`
}

// RegisterRoutes wires tenant routes under /api/platform/tenants. The
// supplied middlewares should include the platform-admin auth check; this
// API must NOT be reachable from per-tenant credentials.
func (h *TenantsHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/platform/tenants", lib.ChainMiddlewares(h.listTenants, middlewares...))
	r.POST("/api/platform/tenants", lib.ChainMiddlewares(h.createTenant, middlewares...))
	r.GET("/api/platform/tenants/{tenant_id}", lib.ChainMiddlewares(h.getTenant, middlewares...))
	r.PUT("/api/platform/tenants/{tenant_id}", lib.ChainMiddlewares(h.updateTenant, middlewares...))
	r.DELETE("/api/platform/tenants/{tenant_id}", lib.ChainMiddlewares(h.deleteTenant, middlewares...))
}

// listTenants — GET /api/platform/tenants
//
// Query params:
//
//	limit=N (default 25, max 100)
//	offset=N (default 0)
//	search=text (case-insensitive substring on id or name)
//	status=active|suspended (exact match)
func (h *TenantsHandler) listTenants(ctx *fasthttp.RequestCtx) {
	params := configstore.TenantsQueryParams{
		Search: string(ctx.QueryArgs().Peek("search")),
		Status: string(ctx.QueryArgs().Peek("status")),
	}
	if v := string(ctx.QueryArgs().Peek("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Limit = n
		}
	}
	if v := string(ctx.QueryArgs().Peek("offset")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Offset = n
		}
	}

	tenants, total, err := h.configStore.GetTenantsPaginated(ctx, params)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to retrieve tenants")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"tenants":     tenants,
		"count":       len(tenants),
		"total_count": total,
		"limit":       params.Limit,
		"offset":      params.Offset,
	})
}

// getTenant — GET /api/platform/tenants/{tenant_id}
func (h *TenantsHandler) getTenant(ctx *fasthttp.RequestCtx) {
	id, ok := ctx.UserValue("tenant_id").(string)
	if !ok || id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "tenant_id is required")
		return
	}
	tenant, err := h.configStore.GetTenant(ctx, id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, "tenant not found")
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to retrieve tenant")
		return
	}
	SendJSON(ctx, map[string]interface{}{"tenant": tenant})
}

// createTenant — POST /api/platform/tenants
func (h *TenantsHandler) createTenant(ctx *fasthttp.RequestCtx) {
	var req CreateTenantRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid JSON")
		return
	}
	if req.ID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "tenant id is required")
		return
	}
	if req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "tenant name is required")
		return
	}
	status := req.Status
	if status == "" {
		status = configstoreTables.TenantStatusActive
	}
	if status != configstoreTables.TenantStatusActive && status != configstoreTables.TenantStatusSuspended {
		SendError(ctx, fasthttp.StatusBadRequest, "status must be 'active' or 'suspended'")
		return
	}

	tenant := &configstoreTables.TableTenant{
		ID:          req.ID,
		Name:        req.Name,
		Status:      status,
		Description: req.Description,
	}
	if err := h.configStore.CreateTenant(ctx, tenant); err != nil {
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, "tenant with this id already exists")
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to create tenant")
		return
	}
	SendJSONWithStatus(ctx, map[string]interface{}{
		"message": "tenant created successfully",
		"tenant":  tenant,
	}, fasthttp.StatusCreated)
}

// updateTenant — PUT /api/platform/tenants/{tenant_id}
//
// Partial update: only the fields supplied in the request body are
// overwritten on the existing row. ID cannot change.
func (h *TenantsHandler) updateTenant(ctx *fasthttp.RequestCtx) {
	id, ok := ctx.UserValue("tenant_id").(string)
	if !ok || id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "tenant_id is required")
		return
	}
	var req UpdateTenantRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid JSON")
		return
	}
	existing, err := h.configStore.GetTenant(ctx, id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, "tenant not found")
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to retrieve tenant")
		return
	}

	if req.Name != nil {
		if *req.Name == "" {
			SendError(ctx, fasthttp.StatusBadRequest, "name cannot be empty")
			return
		}
		existing.Name = *req.Name
	}
	if req.Status != nil {
		if *req.Status != configstoreTables.TenantStatusActive && *req.Status != configstoreTables.TenantStatusSuspended {
			SendError(ctx, fasthttp.StatusBadRequest, "status must be 'active' or 'suspended'")
			return
		}
		existing.Status = *req.Status
	}
	if req.Description != nil {
		existing.Description = *req.Description
	}

	if err := h.configStore.UpdateTenant(ctx, existing); err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "failed to update tenant")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "tenant updated successfully",
		"tenant":  existing,
	})
}

// deleteTenant — DELETE /api/platform/tenants/{tenant_id}
//
// Refuses (409) if the tenant is reserved (default) or still owns
// governance / config rows. Operators wanting "stop billing" semantics
// should PUT status=suspended instead.
func (h *TenantsHandler) deleteTenant(ctx *fasthttp.RequestCtx) {
	id, ok := ctx.UserValue("tenant_id").(string)
	if !ok || id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "tenant_id is required")
		return
	}
	if err := h.configStore.DeleteTenant(ctx, id); err != nil {
		switch {
		case errors.Is(err, configstore.ErrNotFound):
			SendError(ctx, fasthttp.StatusNotFound, "tenant not found")
		case errors.Is(err, configstore.ErrReservedTenant):
			SendError(ctx, fasthttp.StatusConflict, "default tenant cannot be deleted")
		case errors.Is(err, configstore.ErrTenantNotEmpty):
			SendError(ctx, fasthttp.StatusConflict, err.Error())
		default:
			SendError(ctx, fasthttp.StatusInternalServerError, "failed to delete tenant")
		}
		return
	}
	SendJSON(ctx, map[string]interface{}{"message": "tenant deleted successfully"})
}
