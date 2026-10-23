"""Loopback inside the lab network namespace (contract 2.3, D06).

A fresh `unshare --net` namespace has only `lo`, and it starts down. The
flags are read and set with ioctl on a socket of this namespace: /sys/class/net
keeps showing the host's interfaces because sysfs is not remounted.
"""

import fcntl
import socket
import struct

SIOCGIFFLAGS = 0x8913
SIOCSIFFLAGS = 0x8914
IFF_UP = 0x1
_IFREQ = "16sH22x"


def _flags(sock, name):
    req = struct.pack(_IFREQ, name.encode(), 0)
    return struct.unpack(_IFREQ, fcntl.ioctl(sock, SIOCGIFFLAGS, req))[1]


def ensure_up(name="lo"):
    """Bring the interface up if it is down; returns True if it had to."""
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        flags = _flags(s, name)
        if flags & IFF_UP:
            return False
        fcntl.ioctl(s, SIOCSIFFLAGS, struct.pack(_IFREQ, name.encode(), flags | IFF_UP))
        if not _flags(s, name) & IFF_UP:
            raise OSError(f"{name} is still down")
        return True
