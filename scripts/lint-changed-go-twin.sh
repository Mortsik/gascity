#!/usr/bin/env bash
# lint-changed-go-twin.sh — go vet over the packages of changed Go files on
# hosts Bazel does not serve. The go twin of nogo's lint-changed (Makefile):
# CI still gates on bazel.yml's nogo; this keeps the pre-commit gate honest
# where bazel is not installed, the same fallback .githooks/lib/push-suite.sh
# gives the push-time suite.
#
# Packages group by their enclosing Go module (tools/nogo is its own module,
# so its analyzers must vet against that module's go.mod). testdata paths are
# skipped, and a dir without BUILD.bazel is left to `make bazel-sync` to flag
# through the nogo path. bash 3.2 compatible (no associative arrays).
#
# stdin: changed .go paths, one per line (the Makefile's staged/tracked list).
# Exit status: 0 when every go vet pass is clean, 1 otherwise.
set -uo pipefail

repo_root="$(git rev-parse --show-toplevel)" || exit 1
cd "$repo_root" || exit 1

status=0
prev_mod=""
mod_dirs=""

vet_module() {
	# vet_module MOD DIRS...: go vet DIRS in MOD's module root. A package whose
	# files are all behind build tags (a corpus package Bazel builds with its
	# declared tags) reads as "build constraints exclude all Go files" to a
	# plain go vet; that is not a finding, so a pass whose only complaint is
	# tag exclusion warns and moves on. Anything else fails.
	[ -n "$prev_mod" ] || return 0
	echo "lint-changed (go twin): go vet in $prev_mod"
	out="$( cd "$prev_mod" && go vet $mod_dirs 2>&1 )"
	rc=$?
	if [ $rc -ne 0 ]; then
		tagged='build constraints exclude all Go files'
		if printf '%s\n' "$out" | grep -q "$tagged" &&
			! printf '%s\n' "$out" | grep -v "$tagged" | grep -q .; then
			echo "lint-changed (go twin): $prev_mod reports only tag-gated packages (build constraints exclude all Go files); skipping them" >&2
		else
			printf '%s\n' "$out" >&2
			status=1
		fi
	elif [ -n "$out" ]; then
		printf '%s\n' "$out" >&2
	fi
	return 0
}

while IFS= read -r file; do
	[ -n "$file" ] || continue
	case "$file" in
	testdata/* | */testdata/*) continue ;;
	esac
	dir="$(dirname "$file")"
	[ -f "$dir/BUILD.bazel" ] || continue
	mod="$dir"
	while [ "$mod" != "." ] && [ ! -f "$mod/go.mod" ]; do
		mod="$(dirname "$mod")"
	done
	[ -f "$mod/go.mod" ] || continue
	if [ "$dir" != "$mod" ]; then
		dir="${dir#"$mod"/}"
	else
		dir="."
	fi
	if [ "$mod" != "$prev_mod" ]; then
		vet_module
		prev_mod="$mod"
		mod_dirs=""
	fi
	mod_dirs="$mod_dirs ./$dir"
done

vet_module
exit "$status"
