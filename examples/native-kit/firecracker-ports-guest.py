#!/usr/bin/env python3
"""Raw TCP/UDP services in the disposable port-forwarding VM."""
import fcntl
import socket
import struct
import threading

control = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
for ioctl, address in ((0x8916, "172.30.0.2"), (0x891c, "255.255.255.252")):
    fcntl.ioctl(control, ioctl, struct.pack("16sH2s4s8s", b"eth0", socket.AF_INET,
                                         b"\0" * 2, socket.inet_aton(address), b"\0" * 8))
fcntl.ioctl(control, 0x8914, struct.pack("16sH", b"eth0", 0x41))
control.close()

tcp = socket.socket()
tcp.bind(("0.0.0.0", 3000))
tcp.listen()
udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
udp.bind(("0.0.0.0", 3001))
stop = socket.socket()
stop.bind(("0.0.0.0", 3999))
stop.listen()


def stream_echo():
    while True:
        conn, _ = tcp.accept()
        with conn:
            while data := conn.recv(65535):
                conn.sendall(data)


def datagram_echo():
    while True:
        data, address = udp.recvfrom(65535)
        udp.sendto(data, address)


threading.Thread(target=stream_echo, daemon=True).start()
threading.Thread(target=datagram_echo, daemon=True).start()
print("FIRECRACKER_PORTS_READY", flush=True)
conn, _ = stop.accept()
conn.close()
