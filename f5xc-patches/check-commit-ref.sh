#!/usr/bin/env bash
# commit-msg hook: enforce the mandatory Jira `Ref:` trailer (see AGENTS.md →
# "Commit Messages"). Every commit must end with:
#
#     Ref: https://jira.f5net.com/browse/XC-NNNNN
#
# Wired via .pre-commit-config.yaml (stage: commit-msg); pre-commit passes the
# path to the commit-message file as $1.
set -euo pipefail

MSG_FILE="${1:?commit-msg hook expects the message file path as \$1}"

# Strip scissors/comment lines (git's verbose diff, `#` comments) before matching.
body=$(grep -v '^#' "$MSG_FILE" || true)

# Auto-generated commits that legitimately carry no issue ref.
first_line=$(printf '%s\n' "$body" | sed '/^[[:space:]]*$/d' | head -n1)
case "$first_line" in
	Merge\ *|Revert\ *|fixup!\ *|squash!\ *|amend!\ *)
		exit 0 ;;
esac

# Required trailer, e.g. Ref: https://jira.f5net.com/browse/XC-24346
if printf '%s\n' "$body" \
	| grep -Eq '^Ref:[[:space:]]+https://jira\.f5net\.com/browse/[A-Z][A-Z0-9]+-[0-9]+[[:space:]]*$'; then
	exit 0
fi

cat >&2 <<'ERR'
✗ commit rejected: missing Jira reference.

Every commit must end with a Ref trailer on its own line (after a blank line):

    Ref: https://jira.f5net.com/browse/XC-NNNNN

Derive the issue from your branch (e.g. shaun/XC-24346-* → XC-24346).
See AGENTS.md → "Commit Messages".
ERR
exit 1
