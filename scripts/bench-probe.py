"""bench-probe.py WORK_SLICE [MEASURED_UNIT [BESIDE_UNIT]]: what a node's measurement isolation
looks like from its host, as JSON, for bench-check.sh, which runs it there as root over ssh.

WORK_SLICE is the worker's slice (user-1000-hotbed.slice); the measurement's is beside it
(user-1000-hotbed_bench.slice). MEASURED_UNIT is the unit a measurement runs in (a VM's
hotbed-vm-WS.service, or a host command's hotbed-CMD.service), BESIDE_UNIT one of other work.
"""

import json
import os
import subprocess
import sys


def read(path, default=""):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return default


def cpulist(cpus):
    runs = []
    for c in sorted(cpus):
        if runs and runs[-1][1] == c - 1:
            runs[-1][1] = c
        else:
            runs.append([c, c])
    return ",".join(str(a) if a == b else f"{a}-{b}" for a, b in runs)


def parse(text):
    out = set()
    for part in filter(None, text.split(",")):
        a, _, b = part.partition("-")
        out |= set(range(int(a), int(b or a) + 1))
    return out


def mask(text):
    m = int(text.replace(",", ""), 16)
    return {i for i in range(m.bit_length()) if m >> i & 1}


def cgroup(unit):
    path = subprocess.run(
        ["systemctl", "show", "-P", "ControlGroup", unit], capture_output=True, text=True, check=True
    ).stdout.strip()
    return "/sys/fs/cgroup" + path if path else ""


def threads(cg):
    """Each thread of the unit in cgroup cg: its name and the CPUs it may run on."""
    out = []
    for tid in read(cg + "/cgroup.threads").split():
        comm = read(f"/proc/{tid}/comm")
        allowed = ""
        for line in read(f"/proc/{tid}/status").splitlines():
            if line.startswith("Cpus_allowed_list:"):
                allowed = line.split(":", 1)[1].strip()
        if comm:
            out.append({"comm": comm, "cpus": allowed})
    return out


def unit(name):
    cg = cgroup(name)
    if not cg:
        return None
    main = subprocess.run(
        ["systemctl", "show", "-P", "MainPID", name], capture_output=True, text=True, check=True
    ).stdout.strip()
    slice_dir = os.path.dirname(cg)
    return {
        "slice": os.path.basename(slice_dir),
        "partition": read(slice_dir + "/cpuset.cpus.partition"),
        "effective": read(slice_dir + "/cpuset.cpus.effective"),
        "swapMax": read(cg + "/memory.swap.max"),
        "nice": int(read(f"/proc/{main}/stat").rsplit(")", 1)[1].split()[16]) if main not in ("", "0") else None,
        "threads": threads(cg),
    }


def main():
    work_slice = sys.argv[1]
    work = cgroup(work_slice)
    out = {
        "online": read("/sys/devices/system/cpu/online"),
        "workqueues": cpulist(mask(read("/sys/devices/virtual/workqueue/cpumask"))),
        "work": read(work + "/cpuset.cpus.effective"),
        "workIOCapped": any("wbps=max" not in l for l in read(work + "/io.max").splitlines() if l),
        "exclusive": read("/sys/fs/cgroup/user.slice/cpuset.cpus.exclusive"),
        "resctrl": os.path.isdir("/sys/fs/resctrl/hotbed-bench"),
        "clock0": read("/sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq"),
    }
    if out["resctrl"]:
        def l3(path):
            for line in read(path).splitlines():
                if line.strip().startswith("L3:"):
                    return line.split(":", 1)[1].strip()
            return ""
        out["l3"] = [l3("/sys/fs/resctrl/hotbed-bench/schemata"), l3("/sys/fs/resctrl/schemata")]
    if len(sys.argv) > 2:
        out["measured"] = unit(sys.argv[2])
    if len(sys.argv) > 3:
        out["beside"] = unit(sys.argv[3])
    json.dump(out, sys.stdout)


main()
