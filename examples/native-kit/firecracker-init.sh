#!/bin/sh
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /run/e2b-kit
mount -t devpts devpts /dev/pts
chmod 700 /run/e2b-kit
python3 - <<'PY'
import socket,fcntl,struct
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
fcntl.ioctl(s,0x8914,struct.pack('16sH',b'lo',0x49))
PY
/usr/bin/envd --no-cgroups > /tmp/envd.log 2>&1 &
python3 - <<'PY'
import socket,time
for attempt in range(50):
 try:
  socket.create_connection(('127.0.0.1',49983),1).close()
  break
 except OSError: time.sleep(.1)
else: raise RuntimeError('envd did not start')
PY
python3 /smoke.py
result=$?
cat /tmp/envd.log
if [ "$result" = 0 ]; then echo FIRECRACKER_NATIVE_ENVD_OK; else echo FIRECRACKER_NATIVE_ENVD_FAILED; fi
sync
python3 - <<'PYREBOOT'
import ctypes, os
libc = ctypes.CDLL(None, use_errno=True)
if libc.reboot(0x4321fedc) != 0:
    raise OSError(ctypes.get_errno(), 'poweroff')
PYREBOOT
while :; do sleep 1; done
