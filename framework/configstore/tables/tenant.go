package tables

import "time"

// TableTenant is the top-level isolation boundary for multi-tenant Bifrost
// deployments. Every governance entity (customers, teams, virtual keys,
// budgets, rate limits) belongs to exactly one tenant.
//
// Single-tenant OSS deployments use the "default" tenant created by the
// initial tenant migration so existing data remains valid. The HTTP transport
// resolves an inbound virtual key to its owning tenant before dispatching
// to the per-tenant Bifrost runtime; queries inside the governance plugin
// scope by the tenant_id carried in BifrostContext.
//
// Tenants intentionally hold no provider keys, MCP configs, or plugin
// configs of their own — those are still attached to customers/teams/VKs.
// The Tenant table is a *grouping* construct, not a config container.
type TableTenant struct {
	ID     string `gorm:"primaryKey;type:varchar(255)" json:"id"`
	Name   string `gorm:"type:varchar(255);not null" json:"name"`
	Status string `gorm:"type:varchar(32);not null;default:active" json:"status"`

	// Description is a human-readable note shown in the admin UI.
	Description string `gorm:"type:text" json:"description,omitempty"`

	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"index;not null" json:"updated_at"`
}

// TableName sets the table name. We prefix with bf_ rather than governance_
// because tenants are a platform concept above governance, not a governance
// entity themselves.
func (TableTenant) TableName() string { return "bf_tenants" }

// DefaultTenantID is the ID seeded by the initial migration so existing
// single-tenant deployments don't need to opt in to multi-tenancy. All
// governance rows with no explicit tenant are backfilled to this value.
const DefaultTenantID = "default"

// TenantStatus values.
const (
	TenantStatusActive    = "active"
	TenantStatusSuspended = "suspended"
)
