// Tests for the logstore-local GORM tenant-scope callback. Mirrors
// the configstore equivalent (framework/configstore/tenant_scope_test.go);
// the duplication is intentional — see tenant_scope.go's package
// comment for the cycle-breaking rationale.
//
// Coverage:
//   - SELECT against a Log row from tenant A under tenant B's ctx
//     must return ErrNotFound (no cross-tenant log leak).
//   - INSERT under tenant ctx must auto-stamp tenant_id even when the
//     caller forgot to populate the struct field.
//
// This is the regression for the symptom that drove patch6: the UI's
// /api/logs returned every tenant's request history to every tenant
// because the logstore *gorm.DB had no scope callback and the Log
// table had no tenant_id column to filter on.

package logstore

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupRDBLogStoreWithTenantScope creates an in-memory sqlite logstore
// with RegisterTenantScopes wired in. Bypasses triggerMigrations
// (which runs every legacy migration too) because for this test we
// just need the Log table at its current shape.
func setupRDBLogStoreWithTenantScope(t *testing.T) *RDBLogStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err, "open in-memory sqlite")
	require.NoError(t, db.AutoMigrate(&Log{}), "auto-migrate Log")
	require.NoError(t, RegisterTenantScopes(db), "register tenant-scope callbacks")
	return &RDBLogStore{db: db, logger: nil}
}

// withTenant returns a ctx carrying the supplied tenant id under
// multitenant.BifrostContextKeyTenantID — the form the scope callback
// reads. Mirrors the form the HTTP transport's tenant-resolver
// middleware uses.
func withTenant(parent context.Context, tid string) context.Context {
	return context.WithValue(parent, multitenant.BifrostContextKeyTenantID, tid)
}

// TestLogTenantIsolation is the regression for the cross-tenant log
// leak. Tenant A writes a row; tenant B's read under its own ctx must
// not see it; tenant A's read must see only its own row.
func TestLogTenantIsolation(t *testing.T) {
	store := setupRDBLogStoreWithTenantScope(t)
	ctxA := withTenant(context.Background(), "tenantA")
	ctxB := withTenant(context.Background(), "tenantB")

	// Tenant A inserts a log row WITHOUT setting TenantID — the scope
	// callback's INSERT hook must populate it from ctx.
	rowA := &Log{
		ID:        "req-a",
		Timestamp: time.Now(),
		Object:    "chat_completion",
		Provider:  "openai",
		Model:     "gpt-4",
		Status:    "success",
	}
	require.NoError(t, store.Create(ctxA, rowA), "tenant A create")

	// Tenant B inserts under its own ctx.
	rowB := &Log{
		ID:        "req-b",
		Timestamp: time.Now(),
		Object:    "chat_completion",
		Provider:  "anthropic",
		Model:     "claude-3",
		Status:    "success",
	}
	require.NoError(t, store.Create(ctxB, rowB), "tenant B create")

	// Assert tenant_id was populated from ctx on each row (write-side
	// scope works).
	var rawA, rawB Log
	require.NoError(t, store.db.WithContext(context.Background()).
		Where("id = ?", "req-a").First(&rawA).Error)
	require.NoError(t, store.db.WithContext(context.Background()).
		Where("id = ?", "req-b").First(&rawB).Error)
	assert.Equal(t, "tenantA", rawA.TenantID, "row A must carry tenant A's id")
	assert.Equal(t, "tenantB", rawB.TenantID, "row B must carry tenant B's id")

	// Read-side: tenant B reading req-a (tenant A's row) must 404
	// because the scope callback adds `WHERE tenant_id = 'tenantB'`.
	var found Log
	err := store.ScopedDB(ctxB).Where("id = ?", "req-a").First(&found).Error
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound,
		"tenant B must NOT see tenant A's row — this assertion failing means "+
			"the cross-tenant log leak from the bug report is still present.")

	// Tenant A reading its own row succeeds.
	var ownRow Log
	require.NoError(t, store.ScopedDB(ctxA).Where("id = ?", "req-a").First(&ownRow).Error,
		"tenant A must see its own row")
	assert.Equal(t, "openai", ownRow.Provider)
}

// TestLogTenantIsolation_NoCtxTenant covers the single-tenant fallback:
// when no header is on the request, the scope callback must short-
// circuit and the read returns everything. Mirrors the OSS single-
// tenant behaviour patch1 was careful to preserve.
func TestLogTenantIsolation_NoCtxTenant(t *testing.T) {
	store := setupRDBLogStoreWithTenantScope(t)

	// Seed via tenant ctx.
	require.NoError(t, store.Create(withTenant(context.Background(), "tenantA"),
		&Log{ID: "req-a", Timestamp: time.Now(), Provider: "openai", Model: "gpt-4", Status: "success", Object: "chat_completion"}))
	require.NoError(t, store.Create(withTenant(context.Background(), "tenantB"),
		&Log{ID: "req-b", Timestamp: time.Now(), Provider: "anthropic", Model: "claude-3", Status: "success", Object: "chat_completion"}))

	// Read without tenant ctx — scope short-circuits, both rows visible.
	var rows []Log
	require.NoError(t, store.ScopedDB(context.Background()).Find(&rows).Error)
	assert.Len(t, rows, 2, "no-tenant ctx must read every row (single-tenant fallback)")
}

// tableImport keeps the configstore/tables import live — pulled in to
// confirm the same DefaultTenantID literal is reachable from this
// package (the migration backfills logs.tenant_id with this value).
var _ = tables.DefaultTenantID
