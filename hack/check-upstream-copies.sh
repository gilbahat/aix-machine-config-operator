#!/usr/bin/env bash
#
# Checks that the functions copied into pkg/upstream still match the pinned
# machine-config-operator source, apart from the documented renames. Run it
# after moving the MCO pin in go.mod. Needs network access to the Go proxy.
set -euo pipefail
cd "$(dirname "$0")/.."

mod=github.com/openshift/machine-config-operator
ver=$(GOFLAGS=-mod=mod go list -m -f '{{.Version}}' "$mod")
dir=$(GOFLAGS=-mod=mod go mod download -json "$mod@$ver" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
[ -d "$dir" ] || { echo "could not download $mod@$ver" >&2; exit 1; }

# extract FILE... FUNC: print a top-level function from its "func" line to its closing brace.
extract() {
	local fn=$1
	shift
	awk -v f="$fn" '$0 ~ "^func "f"\\(" {p=1} p {print} p && /^}/ {exit}' "$@"
}

status=0
check() { # upstream-file upstream-func local-func
	local up ours
	up=$(extract "$2" "$dir/$1" | sed -e "s/\\b$2\\b/$3/g" -e 's/convertICSPToIDMS(/ConvertICSPToIDMS(/g' -e 's/:= DecodeKubeletConfig(/:= kubeletconfig.DecodeKubeletConfig(/')
	ours=$(for f in pkg/upstream/*.go; do extract "$3" "$f"; done)
	if ! diff <(echo "$up") <(echo "$ours") >/dev/null; then
		echo "pkg/upstream: $3 differs from $mod@$ver $1 $2:" >&2
		diff <(echo "$up") <(echo "$ours") >&2 || true
		status=1
	fi
}

check pkg/controller/container-runtime-config/helpers.go validateRegistriesConfScopes ValidateRegistriesConfScopes
check pkg/controller/container-runtime-config/helpers.go convertICSPToIDMS ConvertICSPToIDMS
check pkg/controller/kubelet-config/helpers.go validateUserKubeletConfig ValidateUserKubeletConfig
[ $status -eq 0 ] && echo "pkg/upstream matches $mod@$ver"
exit $status
