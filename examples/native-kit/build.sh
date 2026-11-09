#!/usr/bin/env bash
# Build the pinned runtime; optionally use an explicit local adapter checkout.
set -euo pipefail
runtime_root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$runtime_root"
if [[ -n ${KIT_E2B_SOURCE:-} ]]; then
  adapter_root=$(cd "$KIT_E2B_SOURCE" && pwd)
  if [[ ! -f "$adapter_root/native.go" ]]; then
    echo "KIT_E2B_SOURCE must name a kits-e2b checkout with native.go" >&2
    exit 1
  fi
  workspace=$(mktemp "$runtime_root/.native-kit.XXXXXX.work")
  trap 'rm -f "$workspace" "$workspace.sum"' EXIT
  cp go.work "$workspace"
  GOWORK="$workspace" go work edit -use="$adapter_root"
  export GOWORK="$workspace"
else
  export GOWORK="$runtime_root/go.work"
fi
export GOPRIVATE="${GOPRIVATE:+$GOPRIVATE,}github.com/docker/kits-e2b"
export GOOS=linux
export GOARCH=${NATIVE_KIT_ARCH:-$(go env GOARCH)}

out="$runtime_root/examples/native-kit/bin/$GOARCH"
mkdir -p "$out"
for service in envd orchestrator api; do
  cgo=0
  if [[ "$service" == orchestrator ]]; then cgo=1; fi
  CGO_ENABLED="$cgo" go -C "packages/$service" build -o "$out/$service" .
done
if [[ ${NATIVE_KIT_TCK:-0} == 1 ]]; then
  go -C packages/orchestrator build -o "$out/native-kit-tck" ./cmd/native-kit-tck
fi
printf 'Native binaries: %s\n' "$out"
