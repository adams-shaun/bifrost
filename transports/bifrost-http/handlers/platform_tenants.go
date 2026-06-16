// Package handlers — platform_tenants.go.
//
// /api/platform/tenants — cross-tenant admin CRUD for managing the
// tenant directory itself.  This is the ONE surface in the API that
// is explicitly NOT tenant-scoped (a platform admin reading the
// list of tenants can't filter by their own tenant; they need to
// see them all).  The tenant-scope GORM callback skips it because
// the `tenants` table doesn't carry a `tenant_id` column.
//
// Auth is platform-admin only.  In single-tenant deployments the
// default seeded tenant is the only row; the surface still works.
package handlers

import (
	"errors"
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// PlatformTenantsHandler implements /api/platform/tenants admin CRUD.
type PlatformTenantsHandler struct {
	configStore configstore.ConfigStore
}

// NewPlatformTenantsHandler constructs a handler.  configStore is required.
func NewPlatformTenantsHandler(configStore configstore.ConfigStore) (*PlatformTenantsHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &PlatformTenantsHandler{configStore: configStore}, nil
}

// RegisterRoutes mounts /api/platform/tenants{,/:id} under the
// supplied middleware chain.  Callers should mount platform-admin
// auth on top so non-admin requests get rejected.
func (h *PlatformTenantsHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/platform/tenants", lib.ChainMiddlewares(h.list, middlewares...))
	r.POST("/api/platform/tenants", lib.ChainMiddlewares(h.create, middlewares...))
	r.GET("/api/platform/tenants/{id}", lib.ChainMiddlewares(h.get, middlewares...))
	r.PUT("/api/platform/tenants/{id}", lib.ChainMiddlewares(h.update, middlewares...))
	r.DELETE("/api/platform/tenants/{id}", lib.ChainMiddlewares(h.delete, middlewares...))
}

type tenantCreateRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
}

type tenantUpdateRequest struct {
	Name        *string `json:"name,omitempty"`
	Status      *string `json:"status,omitempty"`
	Description *string `json:"description,omitempty"`
}

func (h *PlatformTenantsHandler) list(ctx *fasthttp.RequestCtx) {
	var tenants []configstoreTables.TableTenant
	// Plain DB list — no tenant scope (the tenants table has no
	// tenant_id column, so the GORM callback is a no-op here).
	if err := h.configStore.DB().WithContext(ctx).Find(&tenants).Error; err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("list tenants: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{
		"tenants": tenants,
		"count":   len(tenants),
	})
}

func (h *PlatformTenantsHandler) create(ctx *fasthttp.RequestCtx) {
	var req tenantCreateRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if req.ID == "" || req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "id and name are required")
		return
	}
	status := req.Status
	if status == "" {
		status = configstoreTables.TenantStatusActive
	}
	row := &configstoreTables.TableTenant{
		ID:          req.ID,
		Name:        req.Name,
		Status:      status,
		Description: req.Description,
	}
	if err := h.configStore.DB().WithContext(ctx).Create(row).Error; err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("create tenant: %v", err))
		return
	}
	SendJSONWithStatus(ctx, map[string]any{
		"message": "Tenant created",
		"tenant":  row,
	}, fasthttp.StatusCreated)
}

func (h *PlatformTenantsHandler) get(ctx *fasthttp.RequestCtx) {
	id, _ := ctx.UserValue("id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "id is required")
		return
	}
	var row configstoreTables.TableTenant
	if err := h.configStore.DB().WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("tenant %s not found", id))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("get tenant: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"tenant": row})
}

func (h *PlatformTenantsHandler) update(ctx *fasthttp.RequestCtx) {
	id, _ := ctx.UserValue("id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "id is required")
		return
	}
	var req tenantUpdateRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	var row configstoreTables.TableTenant
	if err := h.configStore.DB().WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("tenant %s not found", id))
		return
	}
	if req.Name != nil {
		row.Name = *req.Name
	}
	if req.Status != nil {
		row.Status = *req.Status
	}
	if req.Description != nil {
		row.Description = *req.Description
	}
	if err := h.configStore.DB().WithContext(ctx).Save(&row).Error; err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("update tenant: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{
		"message": "Tenant updated",
		"tenant":  row,
	})
}

func (h *PlatformTenantsHandler) delete(ctx *fasthttp.RequestCtx) {
	id, _ := ctx.UserValue("id").(string)
	if id == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "id is required")
		return
	}
	if id == configstoreTables.DefaultTenantID {
		SendError(ctx, fasthttp.StatusBadRequest, "cannot delete the default tenant")
		return
	}
	if err := h.configStore.DB().WithContext(ctx).Delete(&configstoreTables.TableTenant{}, "id = ?", id).Error; err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("delete tenant: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"message": "Tenant deleted"})
}
