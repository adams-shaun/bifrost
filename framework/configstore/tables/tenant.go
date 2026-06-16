package tables

import "time"

// TableTenant is the top-level isolation boundary for multi-tenant Bifrost
// deployments. Every governance entity (customers, teams, virtual keys,
// budgets, rate limits) and per-tenant config entity (providers, keys,
// MCP clients) belongs to exactly one tenant.
//
// Single-tenant OSS deployments use DefaultTenantID, which is seeded by the
// consolidated tenant migration so existing data remains valid. The HTTP
// transport's tenant-resolver middleware reads the X-Bifrost-Tenant header
// (or derives the tenant from the inbound VK) before dispatching to the
// per-tenant Bifrost runtime; queries inside the governance plugin scope
// by the tenant_id carried in BifrostContext.
//
// Tenants intentionally hold no provider keys, MCP configs, or plugin
// configs of their own — those are still attached to customers / teams /
// VKs / providers. The Tenant table is a *grouping* construct, not a
// config container.
type TableTenant struct {
	ID     string `gorm:"primaryKey;type:varchar(255)" json:"id"`
	Name   string `gorm:"type:varchar(255);not null" json:"name"`
	Status string `gorm:"type:varchar(32);not null;default:active" json:"status"`

	// Description is a human-readable note shown in the admin UI.
	Description string `gorm:"type:text" json:"description,omitempty"`

	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"index;not null" json:"updated_at"`
}

// TableName sets the table name for the tenants registry.
func (TableTenant) TableName() string { return "tenants" }

// DefaultTenantID is the ID seeded by the initial tenant migration so
// existing single-tenant deployments don't need to opt in to multi-tenancy.
// All governance / provider-config rows with no explicit tenant are
// backfilled to this value.
const DefaultTenantID = "default"

// TenantStatus values.
const (
	TenantStatusActive    = "active"
	TenantStatusSuspended = "suspended"
)
