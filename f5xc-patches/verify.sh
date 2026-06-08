#!/usr/bin/env bash
# Strictly validate the F5XC patch series — the gate that would have caught the
# "my change isn't being applied" class of bug.
#
# WHY THIS EXISTS
# ---------------
# `make apply-patches` applies with `git apply --recount`, which *rebuilds* each
# hunk's @@ line counts before applying. That is convenient (it tolerates context
# drift and the `git format-patch` trailing signature) but it is also DANGEROUS:
# a hand-edited .patch with stale/wrong @@ counts still applies locally under
# --recount, so the local build looks fine — while the committed patch is corrupt
# for anyone who replays the series with `git am` (every edit/rebase workflow in
# the README does exactly that). The corruption ships silently.
#
# This script replays every series with `git am` (strict, no --recount) in a
# throwaway worktree under .workspaces/. If a patch was hand-edited, regenerated
# incorrectly, or drifted, `git am` reports "corrupt patch" / "does not apply"
# and we fail LOUDLY — before commit/push.
#
# Run it via `make verify-patches`, or directly. The pre-commit hook runs it
# automatically whenever anything under f5xc-patches/ is staged.
set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT" || exit 1

WORKSPACES="$REPO_ROOT/.workspaces"
WT="$WORKSPACES/verify-patches"
BR="tmp-verify-patches"

red()    { printf '\033[0;31m%s\033[0m\n' "$*"; }
green()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[1;33m%s\033[0m\n' "$*"; }

# --- Precondition: this checkout must be vanilla (patches NOT applied) --------
# verify replays from HEAD; if the working tree already has patches applied the
# replay worktree is still fine, but a present marker means the caller is in the
# stale/applied state we are trying to protect them from — make them clean first.
if [ -f "$REPO_ROOT/f5xc-patches/.applied" ]; then
	red "✗ f5xc-patches/.applied is present — patches are applied in this checkout."
	red "  Run 'make clean-patches' first; verification must run against the vanilla tree."
	exit 1
fi

cleanup() {
	git worktree remove --force "$WT" >/dev/null 2>&1 || true
	git branch -D "$BR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$WORKSPACES"
cleanup
git worktree add -B "$BR" "$WT" HEAD >/dev/null 2>&1

# Series order mirrors the Makefile: sorted dirs that carry an apply.sh
# (so f5xc-patches/security/, which has no apply.sh, is correctly skipped).
mapfile -t SERIES < <(ls -d "$REPO_ROOT"/f5xc-patches/*/*/apply.sh 2>/dev/null | sort | xargs -n1 dirname)

run_replay() {
	cd "$WT" || return 1
	# Identity is passed per-command via `git -c …` rather than `git config`:
	# a linked worktree's `git config` writes to the SHARED .git/config and would
	# pollute the user's real commit identity.
	local d rel p
	for d in "${SERIES[@]}"; do
		rel=${d#"$REPO_ROOT"/}
		echo "==> $rel"
		for p in "$d"/patches/*.patch; do
			printf '    - %-70s ' "$(basename "$p")"
			if git -c user.email=verify@f5xc.local -c user.name='f5xc patch verify' \
				am --no-gpg-sign "$p" >/tmp/f5xc-verify-am.out 2>&1; then
				green "ok"
			else
				red "CORRUPT / does not apply"
				sed 's/^/        /' /tmp/f5xc-verify-am.out
				git am --abort >/dev/null 2>&1 || true
				return 1
			fi
		done
	done
	return 0
}

if run_replay; then
	echo
	green "✓ All F5XC patch series apply cleanly via 'git am' (strict)."
	echo "  Reminder: 'git am' validity ≠ compiles. For the full gate also run:"
	echo "      make clean-patches && make apply-patches && make build"
	exit 0
else
	echo
	red   "✗ PATCH VERIFICATION FAILED."
	echo
	yellow "Most common cause: a .patch file was HAND-EDITED, leaving stale @@ line counts."
	yellow "  - 'make apply-patches' uses 'git apply --recount' and hides this — but"
	yellow "    'git am' (used by every replay/rebase workflow) rejects it as corrupt."
	echo
	yellow "Fix: NEVER hand-edit .patch files. Regenerate the series from a worktree:"
	yellow "  1. git worktree add -B tmp-edit .workspaces/edit HEAD && cd .workspaces/edit"
	yellow "  2. replay with 'git am' up to the patch you want, amend it, then"
	yellow "  3. 're-export with 'git format-patch' and copy back over patches/."
	yellow "  Full procedure: .claude/skills/f5xc-patches/SKILL.md"
	exit 1
fi
