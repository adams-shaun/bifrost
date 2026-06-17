package tables

import (
	"time"

	"gorm.io/gorm"
)

// TableCustomer represents a customer entity with budget and rate limit
type TableCustomer struct {
	ID          string  `gorm:"primaryKey;type:varchar(255)" json:"id"`
	Name        string  `gorm:"type:varchar(255);not null" json:"name"`
	BudgetID    *string `gorm:"type:varchar(255);index" json:"budget_id,omitempty"`
	RateLimitID *string `gorm:"type:varchar(255);index" json:"rate_limit_id,omitempty"`

	// TenantID scopes this customer to a tenant for multi-tenant deployments.
	// Single-tenant OSS uses DefaultTenantID; the consolidated tenant migration
	// backfills existing rows to that value before making the column NOT NULL.
	TenantID string `gorm:"type:varchar(255);not null;default:default;index" json:"tenant_id"`

	// Relationships
	Budget      *TableBudget      `gorm:"foreignKey:BudgetID" json:"budget,omitempty"`
	RateLimit   *TableRateLimit   `gorm:"foreignKey:RateLimitID" json:"rate_limit,omitempty"`
	Teams       []TableTeam       `gorm:"foreignKey:CustomerID" json:"teams"`
	VirtualKeys []TableVirtualKey `gorm:"foreignKey:CustomerID" json:"virtual_keys"`

	// Config hash is used to detect the changes synced from config.json file
	// Every time we sync the config.json file, we will update the config hash
	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"index;not null" json:"updated_at"`
}

// TableName sets the table name for each model
func (TableCustomer) TableName() string { return "governance_customers" }

// BeforeCreate is the per-model multitenant guard. See provider.go's mirror.
func (c *TableCustomer) BeforeCreate(tx *gorm.DB) error {
	return EnsureTenantIDOnCreate(&c.TenantID, "governance_customers")
}
