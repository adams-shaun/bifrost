# f5xc-patches

F5XC-specific overlays for the upstream `maximhq/bifrost` source. The source tree
in this repo is kept **vanilla** — F5XC modifications live as `git format-patch`
files under here and get applied at build time via `make apply-patches`.

This lets the fork stay close to upstream: re-syncing only conflicts when a patch
hunk genuinely overlaps with an upstream change, and the patch series itself
acts as a self-documenting changelog of every F5XC override.

---

## Layout

```
f5xc-patches/
├── README.md
├── .applied                       ← marker (gitignored); holds a CONTENT HASH of the patch set
├── verify.sh                      ← strict `git am` validator (make verify-patches / pre-commit)
├── patch1/
│   └── user-support/
│       ├── apply.sh               ← applies just this series; idempotent within a single run
│       └── patches/
│           ├── 0001-*.patch
│           ├── 0002-*.patch
│           └── ...
└── patch2/
    └── vk-id-header/
        ├── apply.sh
        └── patches/
            ├── 0001-*.patch
            └── ...
```

### Naming convention

- **`patchN/`** — explicit ordering bucket. `patch1/` always runs before
  `patch2/`. Add a new dependent series as `patch3/`, `patch4/`, etc.
- **`<series>/`** — a single cohesive change set. Exactly one series per
  `patchN/` directory; the Makefile globs at `f5xc-patches/*/*/`.
- **`patches/000N-*.patch`** — `git format-patch` output, ordered numerically.

### Why two-level (`patchN/<series>/`) and not flat?

Alphabetical ordering of series names is fragile (`user-support` happens to
sort before `vk-id-header`, but the next series could break that). The
`patchN/` prefix makes ordering explicit and survives renames.

---

## Build integration

The top-level `Makefile` defines:

| Target | What it does |
|---|---|
| `make apply-patches` | Iterates `f5xc-patches/*/*/apply.sh` in sorted order, applies every patch. Records a **content hash** of the patch set in `f5xc-patches/.applied`. Re-running is a no-op while the set is unchanged; if you edit a patch without cleaning first it **errors** ("patch set CHANGED — tree is STALE") instead of silently building stale source. |
| `make clean-patches` | Reverses every patch (in reverse series + reverse patch order), removes the marker. Worktree returns to vanilla. |
| `make verify-patches` | **Strictly** validates the series: replays every patch with `git am` (no `--recount`) in a throwaway `.workspaces/` worktree. Fails loudly on a hand-edited / corrupt / drifted `.patch`. Run before every commit/push that touches `f5xc-patches/`; a pre-commit hook runs it too. |
| `make build` | Depends on `apply-patches` — so a fresh checkout running `make build` ends up with patches applied and the binary built. |

Each series' `apply.sh` uses `git apply --recount --whitespace=nowarn` so:
- Slight context drift is tolerated
- `git format-patch` trailing footers (`-- 2.39.5 (Apple Git-154)`) print a
  benign warning but don't fail the apply

> ⚠️ **The convenience of `--recount` is also a trap.** It *rebuilds* hunk line
> counts, so a **hand-edited** `.patch` with stale `@@` counts still applies here
> — but `git am` (used by every replay/rebase workflow below) rejects it as
> `corrupt patch`. That is exactly how a broken patch shipped to MR !15. **Never
> hand-edit `.patch` files; always `make verify-patches` before committing.**

---

## Common workflows

All three flows below use a **temporary git worktree** so the main checkout
stays in "vanilla + applied" state, and your changes are crystallized as
`.patch` files rather than committed to the branch.

### A. Adding a new patch to an existing series

```bash
REPO=$(pwd)
SERIES=patch1/user-support

# 1. Spin up a temp worktree at vanilla HEAD
git worktree add -B tmp-edit $REPO/.workspaces/edit HEAD
cd $REPO/.workspaces/edit
git config user.email "you@f5.com" && git config user.name "Your Name"

# 2. Replay the existing series via git am (preserves author/message)
for p in $REPO/f5xc-patches/$SERIES/patches/*.patch; do
    git am --no-gpg-sign "$p"
done

# 3. Make your edits, commit normally
vim plugins/governance/...
git add -A
git commit -m "feat(governance): your new thing"

# 4. Export just the new commit as a patch
mkdir -p /tmp/new-patch
git format-patch -1 -o /tmp/new-patch/
cp /tmp/new-patch/*.patch "$REPO/f5xc-patches/$SERIES/patches/"

# 5. Tear down
cd "$REPO"
git worktree remove --force $REPO/.workspaces/edit
git branch -D tmp-edit

# 6. Verify end-to-end
make clean-patches      # safety
make apply-patches      # all patches, including the new one
make build              # full build check
```

### B. Modifying an existing patch in the middle of a series

```bash
REPO=$(pwd)
SERIES=patch1/user-support

git worktree add -B tmp-edit $REPO/.workspaces/edit HEAD
cd $REPO/.workspaces/edit

# Replay the series via git am
for p in $REPO/f5xc-patches/$SERIES/patches/*.patch; do
    git am --no-gpg-sign "$p"
done

# Use interactive rebase to amend the target patch
N=$(ls $REPO/f5xc-patches/$SERIES/patches/*.patch | wc -l)
git rebase -i HEAD~$N
# Mark the target patch as `edit`, save & quit
# Make your edits
git add -A
git commit --amend --no-edit
git rebase --continue

# Re-export the entire series (patch numbers may stay the same; messages too)
rm -rf /tmp/fresh && mkdir /tmp/fresh
git format-patch -$N -o /tmp/fresh/

# Replace the series wholesale
rm "$REPO/f5xc-patches/$SERIES/patches"/*.patch
cp /tmp/fresh/*.patch "$REPO/f5xc-patches/$SERIES/patches/"

cd "$REPO"
git worktree remove --force $REPO/.workspaces/edit
git branch -D tmp-edit

make clean-patches && make apply-patches    # verify
```

### C. Reordering or removing a patch

Same as **B**, but in the interactive rebase, change `pick` lines to `reorder`
them, or delete the line entirely to drop a commit. Then `git format-patch -N`
the new set.

---

## Upstream rebase (absorbing a new `maximhq/bifrost` release)

The fork's integration branch (e.g. `vesdev`) is where upstream lands first;
feature branches sit on top.

```bash
# 1. One-time: add upstream remote
git remote add upstream https://github.com/maximhq/bifrost.git

# 2. Update integration branch from upstream
git checkout vesdev
git fetch upstream
git merge upstream/main          # or rebase, per team policy
git push origin vesdev

# 3. Rebase your feature branch onto the updated integration
git checkout sanju/vk-patch
git rebase vesdev

# 4. Try to apply patches against the new base
make clean-patches               # safe no-op if already clean
make apply-patches               # iterates all series

# 5. If a series fails, rebase just that series
```

### Rebasing a single series after upstream merge

```bash
REPO=$(pwd)
SERIES=patchN/<broken-series>

git worktree add -B tmp-rebase $REPO/.workspaces/rebase HEAD
cd $REPO/.workspaces/rebase

# Apply the series patches one at a time
for p in $REPO/f5xc-patches/$SERIES/patches/*.patch; do
    echo "applying $(basename $p)"
    if ! git am --no-gpg-sign "$p"; then
        # When git am fails:
        git am --abort
        # Use --reject to land what it can, leaves .rej files
        git apply --reject "$p"
        # Hand-resolve the .rej files
        # ... edit files, delete .rej ...
        git add -A
        git commit -m "$(head -1 $p | sed 's/^Subject: //; s/^\[PATCH[^]]*\] //')"
    fi
done

# Re-export the (possibly amended) series
N=$(ls $REPO/f5xc-patches/$SERIES/patches/*.patch | wc -l)
rm -rf /tmp/fresh && mkdir /tmp/fresh
git format-patch -$N -o /tmp/fresh/

rm "$REPO/f5xc-patches/$SERIES/patches"/*.patch
cp /tmp/fresh/*.patch "$REPO/f5xc-patches/$SERIES/patches/"

cd "$REPO"
git worktree remove --force $REPO/.workspaces/rebase
git branch -D tmp-rebase

make clean-patches && make apply-patches
```

### When a patch's intent has been adopted upstream

If upstream merged a fix that obsoletes one of your patches, drop the patch
file from `patches/` and renumber the remaining ones (or leave the gap — the
numbers are just for ordering).

---

## Rules of thumb

- **Keep patches small and single-purpose.** One logical change per commit /
  patch. Easier to rebase when upstream drifts.
- **`make verify-patches` must pass before you commit or push.** It is the strict
  (`git am`) gate that `make apply-patches` (`--recount`) cannot give you. The
  pre-commit hook runs it automatically when a `.patch`/`apply.sh` is staged.
- **Always test the full chain** `make clean-patches && make apply-patches && make build`
  before committing. Catches series-interaction conflicts (e.g. two series both
  modifying `LocalGovernanceStore`) and "applies but doesn't compile".
- **Never hand-edit `.patch` files.** Always regenerate via `git format-patch`.
  Hand-edited patches have stale `@@` line counts: they apply under `--recount`
  but `git am` (and the next upstream merge) rejects them as corrupt.
- **Don't edit source in the main worktree** to make patch changes. The
  apply state mixes with your changes and you'll regenerate broken patches.
  Always use a worktree under `$(pwd)/.workspaces/` (gitignored).
- **Commit only from a vanilla tree.** Before `git commit`: no `f5xc-patches/.applied`,
  no modified vendored source in `git status` — only `.patch`/`apply.sh`/docs.
- **The marker file `f5xc-patches/.applied` is gitignored** — don't try to
  commit it. It holds a content hash of the applied patch set; `apply-patches`
  errors (rather than silently no-op'ing) if you edit a patch without cleaning first.
- **`go.work` is gitignored too** — run `make setup-workspace` after a fresh
  clone (or carry your own `go.work` referencing the modules you need).
