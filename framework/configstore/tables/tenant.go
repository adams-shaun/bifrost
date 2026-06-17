package tables

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// strictAssertTenantEnv mirrors the env var the GORM tenant-scope
// callback in framework/configstore/tenant_scope.go uses. The two
// guards opt into strict mode together via the same flag — set to
// true in production multi-tenant deployments, leave unset in OSS
// tests + single-tenant deployments.
const strictAssertTenantEnv = "BIFROST_ASSERT_TENANT_ON_INSERT"

// EnsureTenantIDOnCreate is the per-model BeforeCreate guard for every
// tenant-scoped table. Behaviour depends on BIFROST_ASSERT_TENANT_ON_INSERT:
//
//   - strict mode (env=true/1/yes/on): errors when *field is empty,
//     stopping the INSERT with a stack-trace-worthy message.
//   - default (env unset or anything else): auto-fills *field with
//     DefaultTenantID and returns nil. Preserves OSS / single-tenant
//     behaviour where rows naturally land in the default tenant.
//
// Either way, no row ever ends up with tenant_id='' on disk — that
// was the silent corruption mode we keep hitting. The mutate-receiver
// shape (*string instead of string) is so the hook can repair the
// field without needing a separate setter.
func EnsureTenantIDOnCreate(field *string, tableName string) error {
	if *field != "" {
		return nil
	}
	envVal := strings.ToLower(strings.TrimSpace(os.Getenv(strictAssertTenantEnv)))
	strict := envVal == "true" || envVal == "1" || envVal == "yes" || envVal == "on"
	if strict {
		return fmt.Errorf("multitenant: %s.tenant_id is required at INSERT but was empty — stamp it at construction (from request ctx, parent entity, or DefaultTenantID for boot-time syncs); BIFROST_ASSERT_TENANT_ON_INSERT=true so this INSERT is blocked", tableName)
	}
	// Non-strict (OSS default) — auto-fill rather than error so existing
	// fixtures and single-tenant deployments keep working untouched.
	*field = DefaultTenantID
	return nil
}

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
