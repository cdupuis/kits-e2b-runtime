#!/usr/bin/env bash
# Isolated real-VM port test. All networking lives in a throwaway namespace.
set -euo pipefail
: "${FIRECRACKER:?path to an architecture-matching Firecracker binary}"
: "${KERNEL:?path to an architecture-matching guest kernel}"
: "${PORTS_TEST:?path to the native kitports Go test binary}"
root=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
ns="kitports-$$"
container=
namespace_created=false
cleanup() {
  if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null; fi
  if [[ "$namespace_created" == true ]]; then
    sudo -n ip netns pids "$ns" | xargs -r sudo -n kill 2>/dev/null || true
    sudo -n ip netns del "$ns"
  fi
  sudo -n rm -rf "$work"
}
trap cleanup EXIT
sudo -n ip netns add "$ns"
namespace_created=true
sudo -n ip -n "$ns" link set lo up
sudo -n ip netns exec "$ns" ip tuntap add dev kit-tap mode tap
sudo -n ip -n "$ns" addr add 172.30.0.1/30 dev kit-tap
sudo -n ip -n "$ns" link set kit-tap up
container=$(docker create python:3.13-slim)
mkdir "$work/rootfs"
docker export "$container" > "$work/rootfs.tar"
sudo -n tar -xf "$work/rootfs.tar" -C "$work/rootfs"
docker rm "$container" >/dev/null
container=
sudo -n cp "$root/firecracker-ports-guest.py" "$work/rootfs/ports.py"
cat > "$work/init" <<'INIT'
#!/bin/sh
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
python3 /ports.py
python3 - <<'PY'
import ctypes
ctypes.CDLL(None).reboot(0x4321fedc)
PY
while :; do sleep 1; done
INIT
sudo -n cp "$work/init" "$work/rootfs/init"
sudo -n chmod +x "$work/rootfs/init"
truncate -s 512M "$work/rootfs.ext4"
sudo -n mkfs.ext4 -q -F -d "$work/rootfs" "$work/rootfs.ext4"
python3 - "$work" "$KERNEL" <<'PY'
import json, sys, os
work, kernel = sys.argv[1:]
config = {"boot-source": {"kernel_image_path": os.path.abspath(kernel),
                        "boot_args": "console=ttyS0 reboot=k panic=1 pci=off init=/init"},
          "drives": [{"drive_id": "rootfs", "path_on_host": work+"/rootfs.ext4", "is_root_device": True, "is_read_only": False}],
          "machine-config": {"vcpu_count": 2, "mem_size_mib": 512},
          "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "kit-tap", "guest_mac": "06:00:00:00:00:01"}]}
json.dump(config, open(work+"/config.json", "w"))
PY
log=${BOOT_LOG:-"$PWD/firecracker-ports-boot.log"}
sudo -n ip netns exec "$ns" timeout 120 "$FIRECRACKER" --no-api --config-file "$work/config.json" > "$log" 2>&1 &
vm=$!
ready=false
for _ in $(seq 1 90); do
  if grep -q '^FIRECRACKER_PORTS_READY' "$log"; then ready=true; break; fi
  if ! kill -0 "$vm" 2>/dev/null; then break; fi
  sleep 1
done
if [[ "$ready" != true ]]; then echo "guest failed to start; see $log" >&2; exit 1; fi
sudo -n ip netns exec "$ns" env KIT_PORT_VM_TARGET=172.30.0.2 "$PORTS_TEST" -test.run '^TestFirecrackerPublishedPorts$' -test.v -test.timeout 20s
sudo -n ip netns exec "$ns" python3 - <<'PY'
import socket
socket.create_connection(("172.30.0.2", 3999), 3).close()
PY
wait "$vm"
if grep -q 'Kernel panic' "$log"; then echo "guest panicked; see $log" >&2; exit 1; fi
echo FIRECRACKER_NATIVE_PORTS_OK
