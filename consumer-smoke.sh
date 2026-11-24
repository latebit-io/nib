#!/bin/bash
# Builds each released module as an external consumer resolves it: outside
# the repo, no replace. Catches a require on an untagged sibling, or code
# that needs a newer sibling than it requires.
#
# Usage: consumer-smoke.sh <module>...   (e.g. ai agent kit)
set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "usage: $0 <module>..." >&2
	exit 2
fi
command -v zip >/dev/null 2>&1 || { echo "error: zip is not installed" >&2; exit 1; }

ROOT=$(cd "$(dirname "$0")" && pwd)
PREFIX=github.com/latebit-io/nib
REAL_PROXY=https://proxy.golang.org
# The version each module under test is staged at. A v0.0.0 prerelease
# sorts below every real tag, so it never outranks a sibling's require.
SMOKE=v0.0.0-smoke

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
PROXY="$WORK/proxy"

# Throwaway module cache: staged versions must never reach the real one.
# GOENV=off makes the empty values stick (else `go env -w` settings apply).
# -trimpath keeps build-cache keys stable across the per-run temp dirs.
export GOMODCACHE="$WORK/modcache"
export GOENV=off
export GOFLAGS="-mod=mod -modcacherw -trimpath"
export GONOSUMDB="$PREFIX"
export GOPRIVATE='' GONOPROXY=''
# Real proxy first: a published sibling version resolves to published code.
# Only versions this commit predicts fall through to the staged copies.
export GOPROXY="$REAL_PROXY,file://$PROXY"

# stage <module> <version>: publish the working tree of <module> into the
# file proxy as <version>.
stage() {
	local mod=$1 ver=$2
	local out="$PROXY/$PREFIX/$mod/@v"
	[ -f "$out/$ver.zip" ] && return 0
	local tree="$WORK/stage/$PREFIX/$mod@$ver"
	mkdir -p "$out" "$tree"
	# Tracked plus untracked-but-not-ignored files: what a commit of the
	# current working tree would publish.
	git -C "$ROOT/$mod" ls-files -co --exclude-standard -z |
		while IFS= read -r -d '' f; do
			[ -f "$ROOT/$mod/$f" ] || continue
			case $f in */*) [ -d "$tree/${f%/*}" ] || mkdir -p "$tree/${f%/*}" ;; esac
			cp "$ROOT/$mod/$f" "$tree/$f"
		done
	(cd "$WORK/stage" && zip -q -r -D -X "$out/$ver.zip" "$PREFIX/$mod@$ver")
	cp "$ROOT/$mod/go.mod" "$out/$ver.mod"
	printf '{"Version":"%s","Time":"2000-01-01T00:00:00Z"}\n' "$ver" >"$out/$ver.info"
	echo "$ver" >>"$out/list"
}

# sibling_requires <module>: print "<sibling> <version>" for every nib
# module the given module's go.mod requires.
sibling_requires() {
	go mod edit -print "$ROOT/$1/go.mod" | awk -v p="$PREFIX/" '
		!/=>/ {
			for (i = 1; i < NF; i++)
				if (index($i, p) == 1 && $(i + 1) ~ /^v[0-9]/)
					print substr($i, length(p) + 1), $(i + 1)
		}'
}

# published <module>: print the module's published versions, one per line.
published() {
	(cd "$WORK" && GOPROXY=$REAL_PROXY go list -m -versions -f '{{join .Versions "\n"}}' "$PREFIX/$1")
}

for mod in "$@"; do
	stage "$mod" "$SMOKE"
	sibling_requires "$mod" | while read -r sibling ver; do
		if [[ " $* " != *" $sibling "* ]]; then
			echo "error: $mod requires $sibling $ver, which is not a released module" >&2
			exit 1
		fi
		tags=$(published "$sibling")
		grep -qxF "$ver" <<<"$tags" && continue # published: the real proxy serves it
		# Unpublished is fine only as a prediction of the next release.
		if [ "$(printf '%s\n' "$tags" "$ver" | sort -V | tail -n 1)" != "$ver" ]; then
			echo "error: $mod requires $sibling $ver: not a published tag, and not newer than the latest ($(tail -n 1 <<<"$tags"))" >&2
			exit 1
		fi
		stage "$sibling" "$ver"
		echo "$ver" >>"$WORK/predicted"
	done
done

# Released modules share one version, so every prediction must agree.
if [ -f "$WORK/predicted" ] && [ "$(sort -u "$WORK/predicted" | wc -l)" -gt 1 ]; then
	echo "error: sibling requires predict different next versions: $(sort -u "$WORK/predicted" | tr '\n' ' ')" >&2
	exit 1
fi

for mod in "$@"; do
	echo "consumer-smoke $mod"
	consumer="$WORK/consumer/$mod"
	mkdir -p "$consumer"
	(
		cd "$consumer"
		go mod init smoke.invalid/consumer >/dev/null 2>&1
		go get "$PREFIX/$mod@$SMOKE"
		go build "$PREFIX/$mod/..."
		go vet "$PREFIX/$mod/..."
	)
done

echo "✓ consumer smoke passed: $*"
