package logstore

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestTenantIDMigrationApplies guards against migrationAddTenantIDColumn
// silently no-opping. It must run as part of triggerMigrations, leave
// the tenant_id column + idx_logs_tenant_id index in place, and stamp
// a row in the migrations table. Without this, the dashboard's
// tenant_id WHERE clause has nothing to match against.
func TestTenantIDMigrationApplies(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := triggerMigrations(context.Background(), db); err != nil {
		t.Fatalf("triggerMigrations: %v", err)
	}
	if !db.Migrator().HasColumn(&Log{}, "tenant_id") {
		t.Fatal("tenant_id column missing after migrations")
	}
	if !db.Migrator().HasIndex(&Log{}, "idx_logs_tenant_id") {
		t.Fatal("idx_logs_tenant_id index missing after migrations")
	}
	var rowCount int64
	if err := db.Table("migrations").Where("id = ?", "logs_add_tenant_id_column").Count(&rowCount).Error; err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("migration row absent, count=%d", rowCount)
	}
}
