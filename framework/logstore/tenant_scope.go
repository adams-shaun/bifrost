// Package logstore — tenant_scope.go.
//
// Mirror of framework/configstore/tenant_scope.go, scoped to this
// package. The logstore and configstore live in the same go module
// (framework/) but import each other in both directions, so we cannot
// import configstore.RegisterTenantScopes here without a cycle.
// Duplicating the ~80 lines of callback logic is cheaper than the
// shared-package refactor.
//
// Contract is identical to configstore's:
//
//   - SELECT against a model with a TenantID column gets an implicit
//     `WHERE <table>.tenant_id = ?` (read from ctx).
//   - UPDATE / DELETE against such a model gets the same predicate.
//   - INSERT against such a model gets tenant_id populated from ctx
//     when the row hasn't already supplied one.
//
// Models without a TenantID column (MCPToolLog, AsyncJob, etc.) flow
// through untouched. Single-tenant fallback (no header on the request,
// background jobs with context.Background()) reads everything as if no
// multi-tenant logic existed — the scope callback short-circuits and
// the column's `default:default` carries the INSERT.

package logstore

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime/debug"
	"strings"

	"github.com/maximhq/bifrost/multitenant"
	"gorm.io/gorm"
)

// tenantColumn is the canonical column name on every tenant-scoped
// logstore model. Kept as a constant so the callback registration
// can't drift from the gorm tags on the Log struct.
const tenantColumn = "tenant_id"

// RegisterTenantScopes installs the four callbacks on the supplied
// *gorm.DB. Must be called exactly once per handle, immediately after
// opening and before business code uses it. Safe to call from both
// sqlite and postgres openers.
func RegisterTenantScopes(db *gorm.DB) error {
	if err := db.Callback().Query().Before("gorm:query").
		Register("multitenant:scope_query", scopeQueryByTenant); err != nil {
		return err
	}
	if err := db.Callback().Update().Before("gorm:update").
		Register("multitenant:scope_update", scopeQueryByTenant); err != nil {
		return err
	}
	if err := db.Callback().Delete().Before("gorm:delete").
		Register("multitenant:scope_delete", scopeQueryByTenant); err != nil {
		return err
	}
	if err := db.Callback().Create().Before("gorm:create").
		Register("multitenant:scope_create", populateTenantIDOnCreate); err != nil {
		return err
	}
	return nil
}

// hasTenantColumn returns true when the model bound to this statement
// carries a `TenantID` column we should scope on. Defensive against
// statements with no Schema parsed (raw SQL, ad-hoc Table queries).
func hasTenantColumn(tx *gorm.DB) bool {
	if tx == nil || tx.Statement == nil || tx.Statement.Schema == nil {
		return false
	}
	return tx.Statement.Schema.LookUpField(tenantColumn) != nil
}

// tenantFromContext returns the tenant id stashed on the request
// context by the tenant-resolver middleware, or empty when no header
// was present. Tries the typed BifrostContextKey first, then the
// string form (fasthttp.RequestCtx.SetUserValue stores by interface{}
// equality so the two key forms don't collide — the resolver writes
// both).
func tenantFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if v := ctx.Value(multitenant.BifrostContextKeyTenantID); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	if v := ctx.Value(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// scopeQueryByTenant is the SELECT / UPDATE / DELETE callback. It
// appends `<table>.tenant_id = ?` to the statement when the model has
// a TenantID column AND the ctx has a tenant id. The column is
// qualified with the primary table name so any future JOIN across
// tenant-scoped tables doesn't throw "ambiguous column name".
func scopeQueryByTenant(tx *gorm.DB) {
	if tx.Error != nil || !hasTenantColumn(tx) {
		return
	}
	tid, ok := tenantFromContext(tx.Statement.Context)
	if !ok {
		return
	}
	tx.Statement.Where(qualifyTenantColumn(tx)+" = ?", tid)
}

// qualifyTenantColumn returns "<primary_table>.tenant_id" when the
// statement's table can be resolved, falling back to bare "tenant_id"
// otherwise.
func qualifyTenantColumn(tx *gorm.DB) string {
	if tx.Statement == nil {
		return tenantColumn
	}
	table := tx.Statement.Table
	if table == "" && tx.Statement.Schema != nil {
		table = tx.Statement.Schema.Table
	}
	if table == "" {
		return tenantColumn
	}
	return table + "." + tenantColumn
}

// populateTenantIDOnCreate is the INSERT callback. Sets TenantID from
// ctx on each row in the statement BEFORE the INSERT runs, when the
// row didn't already carry an explicit value. Bulk inserts (a slice
// of rows) are handled by iterating the reflect value — the logging
// plugin batches with BatchCreateIfNotExists, so this is the hot path.
func populateTenantIDOnCreate(tx *gorm.DB) {
	if tx.Error != nil || !hasTenantColumn(tx) {
		return
	}
	tid, ok := tenantFromContext(tx.Statement.Context)
	if !ok {
		// Mirror of the configstore strict assertion. Fail the INSERT
		// (or log loudly, depending on BIFROST_ASSERT_TENANT_ON_INSERT)
		// when a tenant-scoped log row is being written without a
		// tenant on ctx and the caller didn't stamp TenantID either.
		// Most legitimate callers (batchWriter using plugin root ctx)
		// have TenantID pre-stamped at storeOrEnqueueEntry; the
		// assertion catches the case where they don't.
		assertEveryRowCarriesTenant(tx)
		return
	}
	rv := tx.Statement.ReflectValue
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			setTenantIfEmpty(rv.Index(i), tid)
		}
	case reflect.Struct:
		setTenantIfEmpty(rv, tid)
	}
}

// assertEveryRowCarriesTenant fails the INSERT (or logs, depending on
// BIFROST_ASSERT_TENANT_ON_INSERT) when any row in the batch has an
// empty TenantID and no tenant on ctx. See configstore's mirror for
// the rationale.
func assertEveryRowCarriesTenant(tx *gorm.DB) {
	rv := tx.Statement.ReflectValue
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if !rowHasTenant(rv.Index(i)) {
				raiseTenantAssertion(tx, fmt.Sprintf("INSERT into %s with row #%d.TenantID=\"\" and no tenant on ctx", tx.Statement.Table, i))
				return
			}
		}
	case reflect.Struct:
		if !rowHasTenant(rv) {
			raiseTenantAssertion(tx, fmt.Sprintf("INSERT into %s with TenantID=\"\" and no tenant on ctx", tx.Statement.Table))
		}
	}
}

func rowHasTenant(rv reflect.Value) bool {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return true
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return true
	}
	field := rv.FieldByName("TenantID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return true
	}
	return field.String() != ""
}

// strictAssertEnv opts INTO strict mode (default = log-only). Set to
// true/1/yes/on to convert the assertion from a stderr warning into a
// hard write failure. The Bifrost helm chart sets this in multi-
// tenant deployments; OSS tests + single-tenant deployments leave it
// unset so existing fixture rows that don't carry a tenant continue
// to insert.
const strictAssertEnv = "BIFROST_ASSERT_TENANT_ON_INSERT"

func raiseTenantAssertion(tx *gorm.DB, msg string) {
	stack := debug.Stack()
	envVal := strings.ToLower(strings.TrimSpace(os.Getenv(strictAssertEnv)))
	strict := envVal == "true" || envVal == "1" || envVal == "yes" || envVal == "on"
	if strict {
		_ = tx.AddError(fmt.Errorf("multitenant: tenant assertion failed: %s\n%s", msg, stack))
		return
	}
	fmt.Fprintf(os.Stderr, "[WARN] multitenant: tenant assertion (logged, not blocked): %s\n%s\n", msg, stack)
}

// setTenantIfEmpty sets the TenantID field on rv to tid only when the
// field's current value is the zero string. Honors caller-supplied
// tenant ids (e.g. async jobs that mint logs for a tenant other than
// the one in their own request).
func setTenantIfEmpty(rv reflect.Value, tid string) {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return
	}
	field := rv.FieldByName("TenantID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return
	}
	if field.String() != "" {
		return
	}
	if !field.CanSet() {
		return
	}
	field.SetString(tid)
}
