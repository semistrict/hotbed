#!/usr/bin/env bash
# bench-check.sh NODE: checks measurements (hotbed run --bench) on NODE, through the cluster's own
# hotbed (HOTBED_CONTROLLER), beside whatever else it runs, which it leaves alone. Its commands
# run in two sessions of its own, in workspaces of a repo NODE has built (BENCH_CHECK_REPO,
# default app, of project BENCH_CHECK_PROJECT, default the repo's name), and end with them. What
# the isolation does is looked at from NODE's host (ssh NODE, sudo), where it is, whether the
# measurement runs in a VM (the vm runtime) or on the host: scripts/bench-probe.py says what it
# sees. It takes a few minutes.
#
# What it checks:
#   - a measurement runs on NODE's measurement CPUs (hotbed nodes: benchCpus) alone: its unit in
#     the measurement's slice, a partition of them, every thread of its unit on them (in a VM,
#     each vCPU pinned to its own, in order, and its guest seeing that many CPUs); other work's
#     slice and a command running beside it on the rest; unbound workqueues on the rest; work's
#     IO on the scratch disk capped; where NODE has one, a cache class of its own (resctrl);
#     never swapping, at nice 0;
#   - a second measurement waits for the first, saying so, and runs after;
#   - a measurement whose isolation is broken mid-run (its slice's parent made to hand it one
#     CPU fewer) fails, saying it lost its isolation;
#   - a writer in another session's workspace is kept quiet beside a measurement: held (parked)
#     where no cap reaches it (the VMs' disks), capped elsewhere; the measurement
#     holds and succeeds, and the writer goes on after;
#   - after each, the node is given back: work on every CPU, workqueues too, IO uncapped,
#     nothing exclusive, no cache class left, clocks as they were.
set -euo pipefail
node=${1:?usage: bench-check.sh NODE}
repo=${BENCH_CHECK_REPO:-app}
project=${BENCH_CHECK_PROJECT:-$repo}
here=$(cd "$(dirname "$0")" && pwd)

failures=0
check() { # DESCRIPTION COMMAND...: says whether COMMAND succeeds
  if "${@:2}"; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    failures=$((failures + 1))
  fi
}
# cpus minus A B: the cpulist A less B; droplast A: A less its last CPU; count A: how many; nth A
# I: its I-th CPU; within A: whether every cpulist on stdin is within A.
cpus() {
  python3 -I -c '
import sys
def parse(s):
    out = set()
    for p in filter(None, s.strip().split(",")):
        a, _, b = p.partition("-")
        out |= set(range(int(a), int(b or a) + 1))
    return out
def text(c):
    r = []
    for i in sorted(c):
        if r and r[-1][1] == i - 1:
            r[-1][1] = i
        else:
            r.append([i, i])
    return ",".join(str(a) if a == b else f"{a}-{b}" for a, b in r)
op, a = sys.argv[1], parse(sys.argv[2])
if op == "minus":
    print(text(a - parse(sys.argv[3])))
elif op == "droplast":
    print(text(a - {max(a)}))
elif op == "count":
    print(len(a))
elif op == "nth":
    print(sorted(a)[int(sys.argv[3])])
elif op == "within":
    lists = [l for l in sys.stdin.read().split() if l]
    sys.exit(0 if lists and all(parse(l) <= a for l in lists) else 1)
' "$@"
}

# The sessions are named on every call, bound to nothing: started outside any checkout, and not
# for this T3 thread.
tmp=$(mktemp -d)
cd "$tmp"
unset T3CODE_THREAD_ID HOTBED_SESSION
hb() { hotbed --json "$@"; }
nodejq() { hb nodes | jq -r --arg n "$node" ".data.nodes[] | select(.hostname == \$n) | $1"; }
bench=$(nodejq '.resources.benchCpus // ""')
[[ -n $bench ]] || { echo "bench-check: $node takes no measurements (hotbed nodes: no benchCpus)" >&2; exit 2; }
arch=$(nodejq '.resources.arch')
node_id=$(nodejq '.id')
storage=$(nodejq '.storage')
vm=$(nodejq '(.resources.runtimes // []) | if index("vm") then "yes" else "" end')
# The worker's slices and scratch disk, as its config names them: the program goes on stdin, as
# ssh joins its arguments into one line for the host's shell.
read -r work_slice disk < <(ssh "$node" sudo -n python3 -I - << 'EOF'
import json
w = json.load(open("/var/lib/hotbed/worker/hotbed.json"))["worker"]
print(w.get("slice") or "hotbed.slice", (w.get("bench") or {}).get("disk") or "-")
EOF
) || true
[[ -n ${work_slice:-} ]] || { echo "bench-check: can't read $node's worker config (ssh $node sudo)" >&2; exit 2; }
bench_slice=${work_slice%.slice}_bench.slice
echo "== $node ($arch${vm:+, VMs}), measurement CPUs $bench, slices $work_slice and $bench_slice"

S=$(hb session start -p "$project" | jq -r .data.id)
S2=$(hb session start -p "$project" | jq -r .data.id)
trap 'hotbed session end -s "$S" > /dev/null; hotbed session end -s "$S2" > /dev/null; cd /; rm -rf "$tmp"' EXIT

# runj queues a command in the first session (--detach) and says its ID; runj2 in the second. A
# measurement goes only to a node with measurement CPUs: each session's first run is one, which
# puts its workspace on NODE (checked), where every later run, syncing nothing, stays.
runj() { hb run -s "$S" -r "$repo" --no-sync --arch "$arch" --detach --mem 1G "$@" | jq -r .data.id; }
runj2() { hb run -s "$S2" -r "$repo" --no-sync --arch "$arch" --detach --mem 1G "$@" | jq -r .data.id; }
cmdjq() { hb ps -s "$1" | jq -r --arg id "$2" ".data.commands[] | select(.id == \$id) | $3"; }
state() { cmdjq "$S" "$1" .state; }
state2() { cmdjq "$S2" "$1" .state; }
reason() { cmdjq "$S" "$1" .reason; }
reason2() { cmdjq "$S2" "$1" .reason; }
until_state() { # SESSION ID STATE: waits up to a minute for the command to be in STATE
  for _ in $(seq 300); do [[ $(cmdjq "$1" "$2" .state) == "$3" ]] && return; sleep 0.2; done
}
until_over() { # SESSION ID: waits up to two minutes for the command to end
  for _ in $(seq 600); do
    case $(cmdjq "$1" "$2" .state) in COMMAND_STATE_QUEUED | COMMAND_STATE_RUNNING | COMMAND_STATE_PARKED | COMMAND_STATE_PAUSED) sleep 0.2 ;; *) return ;; esac
  done
}
# unit SESSION ID: the unit a command runs in on the host: its workspace's VM's, or its own.
unit() {
  if [[ -n $vm ]]; then
    echo "hotbed-vm-$(cmdjq "$1" "$2" .workspaceId).service"
  else
    echo "hotbed-$2.service"
  fi
}
probe() { ssh "$node" sudo -n python3 -I - "$work_slice" "$@" < "$here/bench-probe.py"; }
f() { jq -r "$1" "$2"; }

first=$(runj --bench -- true)
first2=$(runj2 --bench -- true)
until_over "$S" "$first"
until_over "$S2" "$first2"
check "the sessions' workspaces are on $node" \
  test "$(cmdjq "$S" "$first" .nodeId) $(cmdjq "$S2" "$first2" .nodeId)" == "$node_id $node_id"

probe > "$tmp/before"
all=$(f .online "$tmp/before")
work_cpus=$(cpus minus "$all" "$bench")
check "work runs on every CPU before ($(f .work "$tmp/before"))" test "$(f .work "$tmp/before")" == "$all"

given_back() { # WHEN: checks the node is as it was before
  probe > "$tmp/after"
  check "$1: work on every CPU again" test "$(f .work "$tmp/after")" == "$(f .work "$tmp/before")"
  check "$1: workqueues too" test "$(f .workqueues "$tmp/after")" == "$(f .workqueues "$tmp/before")"
  check "$1: IO uncapped" test "$(f .workIOCapped "$tmp/after")" == false
  check "$1: nothing exclusive" test "$(f .exclusive "$tmp/after")" == ""
  check "$1: no cache class left" test "$(f .resctrl "$tmp/after")" == false
  check "$1: clocks as they were" test "$(f .clock0 "$tmp/after")" == "$(f .clock0 "$tmp/before")"
}

echo "== a measurement, beside work"
beside=$(runj2 -- sleep 90)
until_state "$S2" "$beside" COMMAND_STATE_RUNNING
# shellcheck disable=SC2016 # For the command's shell.
m=$(runj --bench -- sh -c 'echo "nproc $(nproc)"; sleep 30')
until_state "$S" "$m" COMMAND_STATE_RUNNING
for _ in $(seq 100); do hotbed logs -s "$S" "$m" 2> /dev/null | grep -q '^nproc ' && break; sleep 0.2; done
probe "$(unit "$S" "$m")" "$(unit "$S2" "$beside")" > "$tmp/during"
# A detached command in a VM writes to a terminal: its lines end in \r.
seen=$(hotbed logs -s "$S" "$m" | tr -d '\r' | sed -n 's/^nproc //p')
check "it sees $(cpus count "$bench") CPUs ($seen)" test "$seen" == "$(cpus count "$bench")"
check "its unit is in the measurement's slice ($(f .measured.slice "$tmp/during"))" test "$(f .measured.slice "$tmp/during")" == "$bench_slice"
check "its slice a partition of $bench ($(f '.measured.partition + " " + .measured.effective' "$tmp/during"))" \
  test "$(f '.measured.partition + " " + .measured.effective' "$tmp/during")" == "root $bench"
check "every thread of its unit on $bench ($(f '[.measured.threads[].cpus] | unique | join(" ")' "$tmp/during"))" \
  cpus within "$bench" < <(f '.measured.threads[].cpus' "$tmp/during")
if [[ -n $vm ]]; then
  vcpus=$(f '[.measured.threads[] | select(.comm | startswith("fc_vcpu ")) | (.comm | ltrimstr("fc_vcpu ")) + ":" + .cpus] | sort_by(split(":")[0] | tonumber) | join(" ")' "$tmp/during")
  want=''
  for i in $(seq 0 $(($(cpus count "$bench") - 1))); do want+="${want:+ }$i:$(cpus nth "$bench" "$i")"; done
  check "each vCPU pinned to its own measurement CPU ($vcpus)" test "$vcpus" == "$want"
fi
check "it never swaps (memory.swap.max $(f .measured.swapMax "$tmp/during"))" test "$(f .measured.swapMax "$tmp/during")" == 0
check "at nice $(f .measured.nice "$tmp/during")" test "$(f .measured.nice "$tmp/during")" == 0
check "unbound workqueues on the rest, $work_cpus ($(f .workqueues "$tmp/during"))" test "$(f .workqueues "$tmp/during")" == "$work_cpus"
check "other work's slice on the rest ($(f .work "$tmp/during"))" test "$(f .work "$tmp/during")" == "$work_cpus"
check "a command beside it on the rest ($(f '[.beside.threads[].cpus] | unique | join(" ")' "$tmp/during"))" \
  cpus within "$work_cpus" < <(f '.beside.threads[].cpus' "$tmp/during")
if [[ $disk != - ]]; then
  check "work's IO on the scratch disk capped" test "$(f .workIOCapped "$tmp/during")" == true
fi
if [[ $(f .resctrl "$tmp/during") == true ]]; then
  check "a cache class of its own: $(f '.l3[0]' "$tmp/during"), the others' $(f '.l3[1]' "$tmp/during")" \
    test "$(f '.l3[0]' "$tmp/during")" != "$(f '.l3[1]' "$tmp/during")"
fi
hotbed stop -s "$S2" "$beside" > /dev/null
until_over "$S" "$m"
check "it succeeds ($(state "$m") $(reason "$m"))" test "$(state "$m")" == COMMAND_STATE_SUCCEEDED
given_back "after it"

echo "== two measurements"
first=$(runj --bench -- sleep 15)
until_state "$S" "$first" COMMAND_STATE_RUNNING
second=$(runj2 --bench -- true)
waits=''
for _ in $(seq 50); do
  waits=$(reason2 "$second")
  [[ -n $waits ]] && break
  sleep 0.2
done
check "the second waits: $waits" test "$waits" == "waiting for measurement $first to finish"
until_over "$S2" "$second"
check "both succeed, the second after the first" \
  test "$(state "$first") $(state2 "$second")" == "COMMAND_STATE_SUCCEEDED COMMAND_STATE_SUCCEEDED"
given_back "after them"

echo "== a measurement whose isolation is broken mid-run"
m=$(runj --bench -- sleep 20)
until_state "$S" "$m" COMMAND_STATE_RUNNING
# shellcheck disable=SC2029 # Expanded here, for the host's shell.
ssh "$node" "p=\$(dirname /sys/fs/cgroup\$(systemctl show -P ControlGroup $bench_slice)) && echo $(cpus droplast "$bench") | sudo -n tee \$p/cpuset.cpus.exclusive > /dev/null"
until_over "$S" "$m"
# shellcheck disable=SC2016 # For that shell.
check "it fails, saying it lost its isolation: $(reason "$m")" \
  bash -c '[[ $1 == COMMAND_STATE_FAILED && $2 == *", but the measurement lost its isolation: "* ]]' _ "$(state "$m")" "$(reason "$m")"
given_back "after it"

echo "== a measurement beside a writer in another session's workspace"
writer=$(runj2 -- sh -c 'while :; do dd if=/dev/urandom of=/work/.bench-check-write bs=1M count=256 conv=fsync status=none; done')
until_state "$S2" "$writer" COMMAND_STATE_RUNNING
sleep 5 # so it is writing before the measurement is queued
m=$(runj --bench -- sleep 10)
until_state "$S" "$m" COMMAND_STATE_RUNNING
during=$(state2 "$writer")
held_why=$(reason2 "$writer")
until_over "$S" "$m"
check "the measurement holds and succeeds beside the writer ($(state "$m") $(reason "$m"))" test "$(state "$m")" == COMMAND_STATE_SUCCEEDED
if [[ -n $vm ]]; then
  check "the writer is held during it ($during: $held_why)" test "$during" == COMMAND_STATE_PARKED
else
  check "the writer runs on, capped, during it ($during)" test "$during" == COMMAND_STATE_RUNNING
fi
until_state "$S2" "$writer" COMMAND_STATE_RUNNING
check "the writer goes on after it ($(state2 "$writer"))" test "$(state2 "$writer")" == COMMAND_STATE_RUNNING
hotbed stop -s "$S2" "$writer" > /dev/null
given_back "after them"

echo
if ((failures)); then
  echo "bench-check: $failures failed" >&2
  exit 1
fi
echo "bench-check: all passed"
