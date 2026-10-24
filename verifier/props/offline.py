"""offline: the run happens in a network namespace whose only interface is lo (4, D06).

/sys of the namespace is the host's sysfs: it was mounted outside, so its
class/net keeps listing the host's interfaces. A sysfs mounted from inside
the namespace shows the interfaces of the mounting process's network
namespace, so the property mounts a fresh one on a temporary directory of
the private mount namespace, reads its class/net and unmounts it. It also
asks the kernel through a socket of this namespace (if_nameindex). Both must
list exactly lo. What it read goes to out/runs/offline/interfaces.json, the
property's trace.
"""

import json
import os
import socket
import subprocess
import tempfile

from verifier import report

NAME = "offline"
EXPECTED = ["lo"]


def interfaces(sysfs_root):
    """Interface names under <sysfs>/class/net, sorted."""
    return sorted(os.listdir(os.path.join(sysfs_root, "class", "net")))


def namespace_sysfs_interfaces():
    """class/net of a sysfs mounted inside this namespace."""
    with tempfile.TemporaryDirectory(prefix="mtaproof-sysfs-") as d:
        res = subprocess.run(["mount", "-t", "sysfs", "sysfs", d], capture_output=True, text=True)
        if res.returncode != 0:
            raise RuntimeError(f"mount -t sysfs: {res.stderr.strip()}; run through make verify")
        try:
            return interfaces(d)
        finally:
            subprocess.run(["umount", d], capture_output=True, text=True, check=True)


def check(sysfs_ifaces, kernel_ifaces):
    failures = []
    for source, names in (("sysfs", sysfs_ifaces), ("if_nameindex", kernel_ifaces)):
        if names != EXPECTED:
            failures.append(f"{source} lists {names}; the namespace must have only {EXPECTED}")
    details = {"interfaces": sysfs_ifaces, "if_nameindex": kernel_ifaces}
    return not failures, details, failures


def run(ctx):
    sysfs_ifaces = namespace_sysfs_interfaces()
    kernel_ifaces = sorted(name for _, name in socket.if_nameindex())
    ok, details, failures = check(sysfs_ifaces, kernel_ifaces)
    details["host_sysfs_interfaces"] = interfaces("/sys")
    path = os.path.join(ctx.fresh_dir(NAME), "interfaces.json")
    with open(path, "w", encoding="utf-8") as f:
        f.write(json.dumps(details, indent=2, sort_keys=True) + "\n")
    return report.result(NAME, ok, details, trace=ctx.rel(path), failures=failures)
