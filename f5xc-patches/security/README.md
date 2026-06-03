# f5xc-patches/security/

**Reference copies of security-only changes that are applied directly to the
base source, NOT via `make apply-patches`.**

## Why this directory is different

The rest of `f5xc-patches/<patchN>/<series>/` works by keeping the source tree
vanilla and overlaying changes at build time via `apply.sh`. Security fixes
cannot follow that model because container-image vulnerability scans (Trivy /
Snyk / etc.) read `go.mod` and `package.json` files directly from the checked-
out base tree — they don't know to run `make apply-patches` first. A patch
parked under `f5xc-patches/patch4/` would leave the scan looking at the
vulnerable versions and continue to fail the CI gate.

So security bumps land in the base files directly. This directory is a
**bookkeeping copy of those bumps**, kept in `git format-patch` form so the
audit trail is the same as every other change in `f5xc-patches/`.

## Difference from a regular `patchN/` series

| | regular `patchN/<series>/` | `security/` |
|---|---|---|
| Applied by `make apply-patches` | yes | **no** |
| Touches base source files in git | no (vanilla) | **yes** |
| Used to feed the CI security scanner | n/a | yes (via base files) |
| `apply.sh` present | yes | no — would be a no-op |

The Makefile's `F5XC_PATCH_SERIES := $(sort $(wildcard f5xc-patches/*/*/))`
glob would expand to any two-deep directory under `f5xc-patches/`, so the
patches in this directory live flat (`f5xc-patches/security/000N-….patch`)
rather than under a `patches/` subdirectory. That keeps the glob from
matching `security/` and trying to run a non-existent `apply.sh`.

## Workflow when adding a new security fix

1. Apply the dependency bump directly to the affected `go.mod` / `go.sum` /
   `package.json` / `package-lock.json` files in the base tree.
2. Verify the bump fixes the reported CVE and doesn't break the build / tests.
3. Commit the base-source change with the format
   `security: bump <pkg> <old> -> <new>` and a CVE-IDs-and-severities block in
   the body.
4. `git format-patch -1 HEAD -o f5xc-patches/security/` to drop a reference
   copy alongside the existing ones. Use sequential numbering (0001-, 0002-,
   …).
5. Push the base-source commit through the normal review path; the reference
   patch goes in the same commit so audit trails stay synchronised.

## Index

- `0001-security-bump-x-crypto-…patch` — bumps `golang.org/x/crypto`
  v0.49.0 → v0.52.0 (closes CVE-2026-39830 / -39831 / -39832 / -39833 /
  -39834 / -42508 / -46595), `golang.org/x/net` v0.52.0 → v0.55.0 (closes
  CVE-2026-39821), and `vitest` (with `@vitest/coverage-v8` and `@vitest/ui`)
  2.1.0 → 2.1.9 (closes CVE-2025-24964).
- `0002-security-bump-x-sys-axios-vitest-yaml.patch` — bumps
  `golang.org/x/sys` v0.42.0 → v0.45.0, `axios` 1.15.2 → 1.16.1, `vitest`
  (with `@vitest/coverage-v8` and `@vitest/ui`) 2.1.9/4.0.18 → 4.1.0 (closes
  CVE-2026-47429), and `yaml` 2.6.0 → 2.8.3.
- `0003-security-add-vitejs-plugin-react-to-ui-vitest-config.patch` — wires
  `@vitejs/plugin-react` into `ui/vitest.config.ts`. Required companion to the
  `0002` vitest 4.1.0 bump: vitest 4.1.x honors tsconfig `jsx: "preserve"` and
  stops transforming JSX, which breaks the UI vitest suite (`columns.test.ts`)
  without it. Not a dependency bump — a test-config fix kept here so the
  vitest-4.1.x story stays in one place.
- `0004-security-pin-flatted-3.4.0-3.4.2-in-tests-integratio.patch` — pins
  `flatted` 3.4.0 → 3.4.2 in `tests/integrations/typescript` via an npm
  `overrides` entry (closes CVE-2026-33228, prototype pollution via `parse()`).
  `flatted` is pulled transitively by `@vitest/ui`; the override dedupes the
  nested 3.4.0 copy to the patched 3.4.2.
