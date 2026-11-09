#!/usr/bin/env bash
# Boot patched envd in a disposable real VM without the E2B control plane.
set -euo pipefail
: "${FIRECRACKER:?path to an architecture-matching Firecracker binary}"
: "${KERNEL:?path to an architecture-matching uncompressed guest kernel}"
: "${ENVD:?path to the patched static envd binary}"
root=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
container=
cleanup() {
  if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null; fi
  sudo rm -rf "$work"
}
trap cleanup EXIT
container=$(docker create python:3.13-slim)
mkdir "$work/rootfs"
docker export "$container" > "$work/rootfs.tar"
sudo tar -xf "$work/rootfs.tar" -C "$work/rootfs"
docker rm "$container" >/dev/null
container=
sudo cp "$ENVD" "$work/rootfs/usr/bin/envd"
sudo cp "$root/envd-smoke.py" "$work/rootfs/smoke.py"
sudo cp "$root/firecracker-init.sh" "$work/rootfs/init"
sudo chmod +x "$work/rootfs/init"
# Optional static adapter test binary exercises generated ACP launchers as the
# workload user, including real stdin/stdout and separate stderr.
if [[ -n "${ADAPTER_TEST:-}" ]]; then sudo cp "$ADAPTER_TEST" "$work/rootfs/adapter.test"; fi
truncate -s 512M "$work/rootfs.ext4"
sudo mkfs.ext4 -q -F -d "$work/rootfs" "$work/rootfs.ext4"
python3 - "$work" "$KERNEL" <<'PY'
import json,sys,os
work,kernel=sys.argv[1:]
config={"boot-source":{"kernel_image_path":os.path.abspath(kernel),
        "boot_args":"console=ttyS0 reboot=k panic=1 pci=off init=/init"},
        "drives":[{"drive_id":"rootfs","path_on_host":work+"/rootfs.ext4","is_root_device":True,"is_read_only":False}],
        "machine-config":{"vcpu_count":2,"mem_size_mib":512}}
json.dump(config,open(work+"/config.json","w"))
PY
# The marker, not Firecracker's exit status, proves the guest tests finished.
log=${BOOT_LOG:-"$PWD/firecracker-native-boot.log"}
timeout 180 "$FIRECRACKER" --no-api --config-file "$work/config.json" > "$log" 2>&1
grep -q '^FIRECRACKER_NATIVE_ENVD_OK' "$log"
if grep -q 'Kernel panic' "$log"; then echo "guest panicked; see $log" >&2; exit 1; fi
grep -E 'PASS:|FIRECRACKER_NATIVE_ENVD_OK' "$log"
