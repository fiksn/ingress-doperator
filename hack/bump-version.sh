#!/usr/bin/env bash
# Sets the release version everywhere it is pinned: images, Helm chart, kustomize, e2e test, README.
# Usage: hack/bump-version.sh vX.Y.Z
set -euo pipefail

new="${1:-}"
if [[ ! "$new" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "usage: $0 vX.Y.Z (got '${new}')" >&2
	exit 1
fi

cd "$(dirname "$0")/.."
chart=helm/ingress-doperator-chart/Chart.yaml
old="$(sed -n 's/^appVersion: *//p' "$chart")"
if [[ -z "$old" ]]; then
	echo "could not read appVersion from $chart" >&2
	exit 1
fi
if [[ "$old" == "$new" ]]; then
	echo "already at $new, nothing to bump"
	exit 0
fi

# file|perl substitution; every one must change exactly what it targets. The chart version drops
# the leading v (Helm requires SemVer 2); appVersion and image tags keep it.
edits=(
	"Makefile|s{^(IMG \?= fiksn/ingress-doperator:)\Q$old\E$}{\${1}$new}m"
	"$chart|s{^(version: )v?\Q${old#v}\E$}{\${1}${new#v}}m"
	"$chart|s{^(appVersion: )\Q$old\E$}{\${1}$new}m"
	"helm/ingress-doperator-chart/values.yaml|s{^(  tag: )\Q$old\E$}{\${1}$new}m"
	"config/manager/kustomization.yaml|s{^(  newTag: )\Q$old\E$}{\${1}$new}m"
	"test/e2e/e2e_suite_test.go|s{(\"fiksn/ingress-doperator:)\Q$old\E\"}{\${1}$new\"}"
	"README.md|s{(fiksn/ingress-doperator(?:-webhook)?:)\Q$old\E\b}{\${1}$new}g"
)

# Edit copies first so a missing pattern leaves every file untouched.
work="$(mktemp -d)"
trap 'rm -r "$work"' EXIT
files=()
for edit in "${edits[@]}"; do
	file="${edit%%|*}"
	expr="${edit#*|}"
	if [[ ! -e "$work/$file" ]]; then
		mkdir -p "$work/$(dirname "$file")"
		cp "$file" "$work/$file"
		files+=("$file")
	fi
	before="$(cat "$work/$file")"
	perl -0pi -e "$expr" "$work/$file"
	if [[ "$(cat "$work/$file")" == "$before" ]]; then
		echo "no $old found to replace in $file (pattern: $expr); nothing changed" >&2
		exit 1
	fi
done
for file in "${files[@]}"; do
	cp "$work/$file" "$file"
done

echo "bumped $old -> $new"
