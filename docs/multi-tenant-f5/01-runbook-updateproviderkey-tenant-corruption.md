# Runbook — UpdateProviderKey TenantID corruption (XC-25496)

**Status:** fixed on `f5xc-mt-overlay` (commit `94ec1790b`, regenerated `patch2`).
The fix is **not yet deployed** to any cluster that ran a build older than this commit.

**Field symptom:** `PUT /api/providers/{p}/keys/{kid}` returns
```json
{ "status_code": 404, "error": { "message": "Provider key not found: not found" } }
```
on the second (and every subsequent) update of the same key — even
though the key is still visible in the UI list and on disk in the
sqlite DB.

## Root cause (one paragraph)

`rdb.go`'s `UpdateProviderKey` synthesises a `TableKey` from a stub
`TableProvider` (only `ID` + `Name`) and copies several fields off the
existing row but **never copies `TenantID`**. The synthesised row
carries `TenantID = ""`. `gorm.Save` issues
`UPDATE ... SET tenant_id = '' WHERE id = ? AND tenant_id = '<ctx>'`
— the WHERE comes from the tenant-scope callback, matches the row,
and the SET silently writes empty string into the column. Every
subsequent tenant-scoped read 404s the orphaned row.

Regression coverage:
[`framework/configstore/tenant_scope_test.go`](../../framework/configstore/tenant_scope_test.go)
`TestUpdateProviderKey_PreservesTenantID`.

## Deploy

The fork's local Docker image is built and pushed to the k3d cluster
via the bifrost-aigw chart workflow. To pick up this commit:

```bash
# from /home/sadams/projtmp/bifrost
make clean-patches            # back to vanilla source
make apply-patches            # re-applies the now-fixed patch2
make test-k8s-image           # builds local/bifrost:f5g
k3d image import local/bifrost:f5g -c bm
kubectl -n bifrost-system rollout restart statefulset/bifrost
kubectl -n bifrost-system rollout status statefulset/bifrost --timeout=120s
```

(Adjust target tag / cluster name to match your local conventions.)

## Repair the live DB

The bug has been live since `vesdev-mt-header` first shipped, so any
key UPDATEd through the UI under a non-default tenant header has
`tenant_id=''` on disk. Those rows are invisible to the running app
(every read filters by header tenant). Fix them in place — the rows
themselves are intact, only the `tenant_id` column is wrong.

The repair query is identical for every affected table:

```sql
-- Identify orphan rows so you know which tenant to assign.
SELECT id, name, created_at FROM config_keys
    WHERE tenant_id = '' OR tenant_id IS NULL;
SELECT id, name, created_at FROM config_providers
    WHERE tenant_id = '' OR tenant_id IS NULL;
SELECT id, name, created_at FROM config_mcp_clients
    WHERE tenant_id = '' OR tenant_id IS NULL;
SELECT id, name, created_at FROM governance_virtual_keys
    WHERE tenant_id = '' OR tenant_id IS NULL;
```

For each row, decide its rightful tenant id (compare `created_at`
against when each tenant was onboarded) and:

```sql
UPDATE config_keys SET tenant_id = 'acme'
    WHERE id = <id> AND (tenant_id = '' OR tenant_id IS NULL);
```

If all orphans belong to the same tenant (typical when only one tenant
has been actively using the admin UI), the blanket form is fine:

```sql
UPDATE config_keys SET tenant_id = 'acme'
    WHERE tenant_id = '' OR tenant_id IS NULL;
```

### Repair via the k8s deployment

The live DB lives at `/app/data/config.db` inside the bifrost
StatefulSet pod, on a PVC. Edit it without taking the pod down by
running the repair via a one-off exec — sqlite supports concurrent
readers and a single in-flight WAL writer, so a short UPDATE is safe
without quiescing inference:

```bash
POD=$(kubectl -n bifrost-system get pod -l app=bifrost -o name | head -1)
kubectl -n bifrost-system exec "$POD" -- \
    sqlite3 /app/data/config.db \
    "UPDATE config_keys SET tenant_id = 'acme' WHERE tenant_id = '' OR tenant_id IS NULL;"
```

For invasive repairs (more than a handful of rows, or you want a
backup first), scale the StatefulSet down, take a copy of the PVC's
`/app/data/`, do the SQL, and scale back up.

## Verify

1. From the UI, switch to the affected tenant, open a previously-
   broken provider key, change its model list, click Save.
2. Refresh — the new model list should be present.
3. Repeat the update — it should NOT 404. Before the fix it always
   404'd from the second update onwards.
4. (Optional) tail logs and look for the row corruption pattern that
   used to appear:
   ```
   tenant_id = '' WHERE id = ? AND tenant_id = '<header>'
   ```

## Backout

If for any reason the fix needs to be removed quickly:

```bash
make clean-patches            # reverses all five patches
# tag-pin to a previous good image:
kubectl -n bifrost-system set image statefulset/bifrost bifrost=local/bifrost:<previous-sha>
```

Note: backing out the **fix** while keeping the **schema** (tenant_id
columns + composite uniques) is fine — the bug is in the code path,
not the schema. The orphaned rows you repaired stay repaired even if
you revert the binary.
