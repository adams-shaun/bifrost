---
name: f5xc-patches
description: Safely edit, add, reorder, rebase, and verify the F5XC patch overlays under f5xc-patches/. Use whenever a task touches a .patch file, an apply.sh, the apply-patches/clean-patches/verify-patches Makefile targets, or whenever a "my governance/VK/guardrails change isn't taking effect" symptom points at the patch overlay. Invoked with /f5xc-patches.
allowed-tools: Read, Grep, Glob, Bash, Edit, Write, AskUserQuestion
---

# F5XC Patch Overlay Workflow

The fork keeps the vendored `maximhq/bifrost` source **vanilla** in git. Every F5XC
modification lives as a `git format-patch` file under `f5xc-patches/<patchN>/<series>/patches/`
and is applied **at build time** by `make apply-patches`, then reversed by `make clean-patches`.

This skill is the safe procedure for changing that overlay. It exists because the overlay
has two sharp edges that have already shipped a broken patch to a merge request. **Read
`f5xc-patches/README.md` before doing anything** — this skill is the *safety* layer on top
of it, not a replacement.

---

## The two traps (why this skill is mandatory)

### Trap 1 — Hand-editing a `.patch` file (the one that bit us)

`make apply-patches` applies with `git apply --recount`, which **rebuilds** each hunk's
`@@ -a,b +c,d @@` line counts before applying. So if you open a `.patch` and type new `+`
lines directly into a hunk, it **still applies locally** — the build looks green.

But `--recount` is the *only* thing tolerating it. Every replay/rebase workflow (and CI's
stricter paths) uses **`git am`**, which does **not** recount and rejects the file as
`corrupt patch at line N`. The corruption ships silently in the committed `.patch` and
breaks the next person who tries to replay the series.

> **Real incident (MR !15):** commit `6224d703d` added 5 `+` lines straight into
> `patch2/.../0004-...-VK-resolution-on-ID-vs-value.patch`. It applied under `--recount`,
> so local tests passed — but `git am` reports `corrupt patch`, so the VK-ID value-injection
> change could not be cleanly replayed and behaved as "not being applied properly."

**Rule: never hand-edit a `.patch` file. Always regenerate it via `git format-patch` from a
worktree.** (`Edit`/`Write` on a `*.patch` under `f5xc-patches/` is forbidden.)

### Trap 2 — The `.applied` marker is a state flag, not a content flag

`f5xc-patches/.applied` answers *"did an apply ever run?"* — but callers read it as *"is the
**current** patch set applied?"*. Those differ. The Makefile now stores a **content hash** of
the patch set in the marker and **errors loudly** if you edit a patch and re-run
`make apply-patches` without cleaning first (older checkouts silently no-op'd and built stale
source). Even so:

**Rule: after changing any patch, always `make clean-patches` before `make apply-patches`.**
Never trust an existing marker. When in doubt, `make clean-patches` is a safe no-op.

---

## Golden rules (apply to every agent, every time)

1. **Never `Edit`/`Write` a `*.patch` file.** Regenerate via `git format-patch`.
2. **Never edit vendored source in the main worktree** to make a patch change — your edits
   mix with the applied state and you export a broken patch. Use a throwaway worktree.
3. **Work in `$(pwd)/.workspaces/<name>`** (gitignored), not `/tmp` — keeps worktrees with the
   repo, on the same filesystem, and out of `git status`. **Assume other agents share this
   clone:** stay in your worktree, never touch the main checkout, and **branch from an explicit
   ref — never `HEAD`** (HEAD follows the shared checkout and another agent can switch it under
   you). Track your base reference (branch + SHA). See AGENTS.md → "Working in a shared repo".
4. **Clean before apply** after any patch change: `make clean-patches && make apply-patches`.
5. **`make verify-patches` must pass before you commit or push** anything under `f5xc-patches/`.
6. **Commit from a vanilla tree.** Before `git commit`, confirm `f5xc-patches/.applied` is
   absent and `git status` shows no modified vendored source — only `.patch`/`apply.sh`/docs.

---

## The mandatory verification sequence

Run this after ANY change to the overlay, and before every commit/push. It is the series of
checks that distinguishes "applies on my machine" from "actually valid."

```bash
# 1. Strict validity — replays every series with `git am` (no --recount).
#    Catches hand-edits, stale counts, corruption, drift. This is the gate that
#    would have caught MR !15.
make verify-patches

# 2. Round-trip + compile — proves the set applies cleanly AND builds.
make clean-patches
make apply-patches        # errors loudly if the set changed since last apply
make build                # `git am` validity does NOT imply it compiles

# 3. Return the tree to vanilla before committing.
make clean-patches
git status --short        # expect: only your .patch / apply.sh / doc changes, NO vendored source
test ! -f f5xc-patches/.applied && echo "OK: vanilla, safe to commit"
```

If step 1 fails with `corrupt patch`, a `.patch` was hand-edited or hand-fixed — do **not**
try to hand-fix the counts. Regenerate the series (recipe below).

---

## Recipes (all use `.workspaces/`, all end by re-exporting via `git format-patch`)

Set up once per task:

```bash
REPO=$(git rev-parse --show-toplevel)
SERIES=patch2/vk-id-header        # the series you are changing
BASE=vesdev                       # YOUR explicit base ref — never HEAD (a shared checkout
                                  # may be on another agent's branch). Pin it, track it.
WT="$REPO/.workspaces/edit"

git worktree add -B tmp-edit "$WT" "$BASE"
cd "$WT"
# Identity per-command (below) — do NOT `git config` in a worktree; it writes the shared
# .git/config and clobbers another agent's identity. Prefer `git -c user.name=… -c user.email=…`.

# Replay the existing series so HEAD == vanilla + this series, commit-by-commit.
for p in "$REPO/f5xc-patches/$SERIES/patches"/*.patch; do
    git am --no-gpg-sign "$p"        # if THIS fails, the committed series is already corrupt
done
```

### A. Add a new patch to a series
```bash
# ...edit source, then:
git add -A && git commit -m "feat(governance): your change"
git format-patch -1 -o "$REPO/f5xc-patches/$SERIES/patches/"   # writes the next NNNN-*.patch
```

### B. Modify an existing patch mid-series
```bash
N=$(ls "$REPO/f5xc-patches/$SERIES/patches"/*.patch | wc -l)
git rebase -i HEAD~$N              # mark the target commit `edit`
# ...make edits...
git add -A && git commit --amend --no-edit && git rebase --continue
# Re-export the WHOLE series (numbers/messages stay stable):
rm "$REPO/f5xc-patches/$SERIES/patches"/*.patch
git format-patch -$N -o "$REPO/f5xc-patches/$SERIES/patches/"
```

### C. Reorder / remove a patch
Same as B, but in the `git rebase -i` list reorder or delete the line, then re-export `-$N`.

### Tear down (every recipe)
```bash
cd "$REPO"
git worktree remove --force "$WT"
git branch -D tmp-edit
# then run THE MANDATORY VERIFICATION SEQUENCE above.
```

---

## Self-check before you say "done"

- [ ] I did not `Edit`/`Write` any `*.patch` file; every patch came from `git format-patch`.
- [ ] `make verify-patches` passed (strict `git am` replay).
- [ ] `make clean-patches && make apply-patches && make build` succeeded.
- [ ] Tree is vanilla: no `f5xc-patches/.applied`, no modified vendored source in `git status`.
- [ ] The diff I'm committing contains only `.patch` / `apply.sh` / docs — not applied source.

If any box is unchecked, the change is not done.
