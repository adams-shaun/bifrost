// Package configstore — tenant_scope.go.
//
// The single GORM callback registration that makes every existing
// query and write tenant-aware without per-method edits.
//
// Contract
// --------
// The HTTP layer (transports/.../middlewares.go's tenant-resolver
// middleware) reads the `x-f5xc-tenant` request header and stashes
// the value on the request context under
// multitenant.BifrostContextKeyTenantID.  All configstore methods
// accept a context.Context that flows through to gorm via
// db.WithContext(ctx).  Once the callbacks below are registered on a
// *gorm.DB, every:
//
//   - SELECT against a model that has a `TenantID` column gets an
//     implicit `WHERE tenant_id = ?` (read from ctx)
//   - INSERT against such a model gets an implicit `tenant_id = ?`
//     populated (read from ctx) when the row hasn't supplied one
//   - UPDATE / DELETE against such a model gets an implicit
//     `WHERE tenant_id = ?` so a tenant cannot mutate another
//     tenant's rows even if the row ID would otherwise match
//
// Models WITHOUT a `TenantID` column flow through untouched.
//
// Single-tenant fallback
// ----------------------
// When ctx carries no tenant_id (cron jobs, internal background
// work, single-tenant deployments where the header isn't set), the
// schema's `default:default` column default takes over on INSERT and
// the scope predicate is simply skipped — so a single-tenant
// deployment with the header absent reads everything as if there
// were no multi-tenant logic at all.

package configstore

import (
	"context"
	"reflect"

	"github.com/maximhq/bifrost/multitenant"
	"gorm.io/gorm"
)

// tenantColumn is the canonical column name on every tenant-scoped
// model.  Kept as a constant so the callback registration doesn't
// drift from the gorm tags on the table structs.
const tenantColumn = "tenant_id"

// RegisterTenantScopes installs the SELECT / INSERT / UPDATE / DELETE
// callbacks that read tenant identity from the request context and
// either constrain the query (reads / mutations of existing rows) or
// populate the column (new rows).
//
// MUST be called exactly once per *gorm.DB, immediately after open
// and before any business code uses the handle.  Safe to call from
// both sqlite and postgres constructors.
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
// carries a `TenantID` column we should scope on.  Defensive against
// statements with no Schema parsed (raw SQL, ad-hoc Table queries).
func hasTenantColumn(tx *gorm.DB) bool {
	if tx == nil || tx.Statement == nil || tx.Statement.Schema == nil {
		return false
	}
	return tx.Statement.Schema.LookUpField(tenantColumn) != nil
}

// tenantFromContext returns the tenant id stashed on the request
// context by the tenant-resolver middleware, or empty when no header
// was present (in which case the column default + lack of WHERE
// predicate combine to give single-tenant semantics).
//
// The middleware stores the value under TWO forms because of
// fasthttp + Go interface{} key semantics:
//
//   - the typed schemas.BifrostContextKey (preferred — used by
//     BifrostContext.Value lookups on the inference path)
//   - the stringified form (because fasthttp.RequestCtx.SetUserValue
//     stores by interface{} equality, and a string key doesn't match
//     a typed-string key on lookup; ctx.Value(typedKey) misses what
//     ctx.SetUserValue(stringKey, ...) put there)
//
// Try the typed key first, fall back to the string form.
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

// scopeQueryByTenant is the SELECT / UPDATE / DELETE callback.  It
// appends a `<table>.tenant_id = ?` predicate to the statement when
// the model has a TenantID column AND the ctx has a tenant id.
//
// The column is qualified with the primary table name so JOINs across
// two tenant-scoped tables (e.g. config_keys -> config_providers,
// both of which carry tenant_id) don't throw "ambiguous column name:
// tenant_id" at the DB. The qualification falls back to bare
// `tenant_id` only when the statement's primary table isn't known
// (raw SQL paths that bypassed the schema parser).
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
// otherwise. Centralised so future callbacks that need the same
// JOIN-safe column ref reuse it.
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

// populateTenantIDOnCreate is the INSERT callback.  Sets TenantID
// from ctx on each row in the statement BEFORE the INSERT runs, when
// the row didn't already carry an explicit value.  Bulk inserts (a
// slice of rows) are handled by iterating the reflect value.
func populateTenantIDOnCreate(tx *gorm.DB) {
	if tx.Error != nil || !hasTenantColumn(tx) {
		return
	}
	tid, ok := tenantFromContext(tx.Statement.Context)
	if !ok {
		return
	}
	rv := tx.Statement.ReflectValue
	ctx := tx.Statement.Context
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			setTenantIfEmpty(ctx, rv.Index(i), tid)
		}
	case reflect.Struct:
		setTenantIfEmpty(ctx, rv, tid)
	}
}

// setTenantIfEmpty sets the TenantID field on rv to tid only when the
// field's current value is the zero string.  Honors caller-supplied
// tenant ids (e.g. /api/platform/tenants admin handlers that mint
// rows on behalf of a tenant other than the one in their own
// request) — gorm.Field.Set has reasonable semantics for the string
// case but we gate on emptiness ourselves for clarity.
func setTenantIfEmpty(_ context.Context, rv reflect.Value, tid string) {
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
