#!/bin/bash
# Compares the stable surface (STABILITY.md) with the last release this
# tree contains. An incompatible change fails unless the title marks it.
set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "usage: $0 <module>...   (e.g. ai agent kit)" >&2
	exit 2
fi
command -v apidiff >/dev/null 2>&1 || {
	echo "error: apidiff is not installed (version pinned in .github/workflows/api-compat.yml)" >&2
	exit 1
}

ROOT=$(cd "$(dirname "$0")" && pwd)
PREFIX=github.com/latebit-io/nib
cd "$ROOT"

# The title checked for a break marker: API_CHECK_TITLE (CI passes the
# pull request title), else HEAD's subject.
TITLE=${API_CHECK_TITLE:-$(git log -1 --format=%s)}

# The nearest release at or before API_CHECK_FROM (default HEAD), so a
# branch behind main is not compared with a release it has not merged.
# Empty or all zeros (GitHub's "no previous commit") also means HEAD.
FROM=${API_CHECK_FROM:-}
[ -n "${FROM//0/}" ] || FROM=HEAD
BASE=$(git describe --tags --match 'agent/v*' --abbrev=0 "$FROM") || {
	echo "error: no agent/v* tag reachable from $FROM (shallow clone?)" >&2
	exit 1
}
VERSION=${BASE#agent/}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
git archive "$BASE" | tar -x -C "$WORK"
OLD="$WORK/old.export"

# covered <module>: go list patterns for the module's stable packages
# (STABILITY.md). Only kit exports packages outside the surface.
covered() {
	case "$1" in
	kit) echo ". ./event" ;;
	*) echo "./..." ;;
	esac
}

breaks=""
for mod in "$@"; do
	# A module first released after the base has nothing to compare.
	[ -d "$WORK/$mod" ] || continue
	# Listed from the old tree: a removed package is seen, and a new one,
	# which cannot break anyone, is skipped.
	paths=$(cd "$WORK/$mod" && go list $(covered "$mod") | grep -Ev '/internal(/|$)')
	current=$(cd "$ROOT/$mod" && go list ./...)
	for path in $paths; do
		name=${path#"$PREFIX/"}
		# Membership, not directory: a package can lose its Go files
		# while its directory stays.
		if ! grep -qxF "$path" <<<"$current"; then
			breaks="$breaks$name: package removed"$'\n'
			continue
		fi
		(cd "$WORK/$mod" && apidiff -w "$OLD" "$path")
		# With -incompatible, apidiff prints only breaking changes.
		out=$(cd "$ROOT/$mod" && apidiff -incompatible "$OLD" "$path")
		[ -z "$out" ] || breaks="$breaks$name:"$'\n'"$out"$'\n'
	done
done

if [ -z "$breaks" ]; then
	echo "api-check: stable surface is compatible with $VERSION"
	exit 0
fi
printf 'api-check: incompatible with %s:\n%s' "$VERSION" "$breaks"

# Break marker: "!" before the colon, any type or scope. release.yml's
# minor_pattern carries the same regex; keep the two in step.
if printf '%s' "$TITLE" | grep -Eq '^[a-z]+(\([^)]+\))?!:'; then
	echo "api-check: allowed, the title marks the break: $TITLE"
	exit 0
fi
echo "api-check: a deliberate break needs \"!\" in the pull request title, as in feat(llm)!: ...; see STABILITY.md" >&2
exit 1
