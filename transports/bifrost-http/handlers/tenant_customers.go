// Package handlers — tenant-scoped governance customer admin.
//
// Mounts CRUD under /api/tenants/{tenant_id}/governance/customers. Like
// teams, customers are tagging/grouping metadata so the runtime evictor
// pattern from the inference-plane handlers is intentionally not wired
// here.
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

type TenantCustomersHandler struct {
	configStore configstore.ConfigStore
}

func NewTenantCustomersHandler(configStore configstore.ConfigStore) (*TenantCustomersHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantCustomersHandler{configStore: configStore}, nil
}

func (h *TenantCustomersHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h.configStore == nil {
		return
	}
	r.GET("/api/tenants/{tenant_id}/governance/customers", lib.ChainMiddlewares(h.listCustomers, middlewares...))
	r.POST("/api/tenants/{tenant_id}/governance/customers", lib.ChainMiddlewares(h.createCustomer, middlewares...))
	r.GET("/api/tenants/{tenant_id}/governance/customers/{customer_id}", lib.ChainMiddlewares(h.getCustomer, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/governance/customers/{customer_id}", lib.ChainMiddlewares(h.updateCustomer, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/governance/customers/{customer_id}", lib.ChainMiddlewares(h.deleteCustomer, middlewares...))
}

type CreateTenantCustomerRequest struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

type UpdateTenantCustomerRequest struct {
	Name *string `json:"name,omitempty"`
}

func (h *TenantCustomersHandler) listCustomers(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	customers, err := h.configStore.GetCustomersByTenant(ctx, string(tid))
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list customers: %v", err))
		return
	}
	// Match GetCustomersResponse — UI reads response.customers +
	// count + total_count + limit + offset.
	SendJSON(ctx, map[string]any{
		"customers":   customers,
		"count":       len(customers),
		"total_count": len(customers),
		"limit":       len(customers),
		"offset":      0,
	})
}

func (h *TenantCustomersHandler) createCustomer(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	var req CreateTenantCustomerRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "name is required")
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	customer := &configstoreTables.TableCustomer{
		ID:   req.ID,
		Name: req.Name,
	}
	if err := h.configStore.CreateCustomerForTenant(ctx, string(tid), customer); err != nil {
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Customer %s already exists for tenant %s", req.Name, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create customer: %v", err))
		return
	}
	// {message, customer} envelope matches the legacy
	// /api/governance/customers POST so UI's createCustomer mutation
	// (typed as {message: string; customer: Customer}) round-trips.
	SendJSONWithStatus(ctx, map[string]any{
		"message":  "Customer created successfully",
		"customer": customer,
	}, fasthttp.StatusCreated)
}

func (h *TenantCustomersHandler) getCustomer(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	customerID, _ := ctx.UserValue("customer_id").(string)
	if customerID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "customer_id is required")
		return
	}
	customer, err := h.configStore.GetCustomerByIDForTenant(ctx, string(tid), customerID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Customer %s not found for tenant %s", customerID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get customer: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"customer": customer})
}

func (h *TenantCustomersHandler) updateCustomer(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	customerID, _ := ctx.UserValue("customer_id").(string)
	if customerID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "customer_id is required")
		return
	}
	var req UpdateTenantCustomerRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Name == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "name is required")
		return
	}
	existing, err := h.configStore.GetCustomerByIDForTenant(ctx, string(tid), customerID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Customer %s not found for tenant %s", customerID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load customer: %v", err))
		return
	}
	existing.Name = *req.Name
	if err := h.configStore.UpdateCustomerForTenant(ctx, string(tid), existing); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Customer %s not found for tenant %s", customerID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update customer: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{
		"message":  "Customer updated successfully",
		"customer": existing,
	})
}

func (h *TenantCustomersHandler) deleteCustomer(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	customerID, _ := ctx.UserValue("customer_id").(string)
	if customerID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "customer_id is required")
		return
	}
	if err := h.configStore.DeleteCustomerForTenant(ctx, string(tid), customerID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Customer %s not found for tenant %s", customerID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete customer: %v", err))
		return
	}
	SendJSON(ctx, map[string]any{"message": "Customer deleted successfully"})
}
