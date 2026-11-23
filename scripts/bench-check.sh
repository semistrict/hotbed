#!/usr/bin/env bash
# bench-check.sh NODE: checks measurements (hotbed run --bench) on NODE, through the cluster's own
# hotbed (HOTBED_CONTROLLER), beside whatever else it runs, which it leaves alone. Its commands
# run in two sessions of its own, in workspaces of a repo NODE has built (BENCH_CHECK_REPO,
# default app, of project BENCH_CHECK_PROJECT, default the repo's name), and end with them.
# It takes about a minute.
#
# What it checks:
#   - a measurement runs on NODE's measurement CPUs (hotbed nodes: benchCpus) alone: its slice a
#     partition of them, other work's slice and a command running beside it on the rest;
#     unbound workqueues on the rest; work's IO on the scratch disk capped; where NODE has one,
#     a cache class of its own (resctrl); never swapping, at nice 0;
#   - a second measurement waits for the first, saying so, and runs after;
#   - a measurement whose isolation is broken mid-run (its slice's parent made to hand it one
#     CPU fewer, with sudo) fails, saying it lost its isolation;
#   - a writer in another session's workspace, on the pool or disk NODE keeps volumes on, is kept
#     quiet beside a measurement: held (parked) on ZFS, which no cap reaches, capped elsewhere; the
#     measurement holds and succeeds, and the writer goes on after;
#   - after each, the node is given back: work on every CPU, workqueues too, IO uncapped,
#     nothing exclusive, no cache class left, clocks as they were.
set -euo pipefail
node=${1:?usage: bench-check.sh NODE}
repo=${BENCH_CHECK_REPO:-app}
project=${BENCH_CHECK_PROJECT:-$repo}

failures=0
check() { # DESCRIPTION COMMAND...: says whether COMMAND succeeds
  if "${@:2}"; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    failures=$((failures + 1))
  fi
}
# cpus minus A B: the cpulist A less B; cpus droplast A: A less its last CPU.
cpus() {
  python3 -I - "$@" <<'EOF'
import sys
def parse(s):
    out = set()
    for p in filter(None, s.split(",")):
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
print(text(a - parse(sys.argv[3])) if op == "minus" else text(a - {max(a)}))
EOF
}

# The sessions are named on every call, bound to nothing: started outside any checkout, and not
# for this T3 thread.
tmp=$(mktemp -d)
cd "$tmp"
unset T3CODE_THREAD_ID HOTBED_SESSION
hb() { hotbed --json "$@"; }
S=$(hb session start -p "$project" | jq -r .data.id)
S2=$(hb session start -p "$project" | jq -r .data.id)
trap 'hotbed session end -s "$S" > /dev/null; hotbed session end -s "$S2" > /dev/null; cd /; rm -rf "$tmp"' EXIT
bench=$(hb nodes | jq -r --arg n "$node" '.data.nodes[] | select(.hostname == $n) | .resources.benchCpus // ""')
[[ -n $bench ]] || { echo "bench-check: $node takes no measurements (hotbed nodes: no benchCpus)" >&2; exit 2; }
echo "== $node, measurement CPUs $bench"

# run is an attached run in the first session, printing the command's output; runj one whose
# envelope says what it queued (--detach).
run() { hotbed run -s "$S" -r "$repo" --node "$node" --no-sync "$@"; }
runj() { hb run -s "$S" -r "$repo" --node "$node" --no-sync "$@"; }
run2() { hotbed run -s "$S2" -r "$repo" --node "$node" --no-sync "$@"; }
state() { hb ps -s "$S" | jq -r --arg id "$1" '.data.commands[] | select(.id == $id) | .state'; }
reason() { hb ps -s "$S" | jq -r --arg id "$1" '.data.commands[] | select(.id == $id) | .reason'; }
field() { awk -v k="$1" '$1 == k { $1 = ""; sub(/^ /, ""); print }' "$2"; }

# The commands' shared start: where things are, as a command finds them.
# shellcheck disable=SC2016 # For the commands' shells.
lib='set -euo pipefail
cg=/sys/fs/cgroup$(sed -n "s/^0:://p" /proc/self/cgroup)
slice=${cg%/*}
mine() { sed -n "s/^Cpus_allowed_list:[[:space:]]*//p" /proc/self/status; }
mask() { python3 -Ic "import sys; m = int(sys.argv[1].replace(\",\", \"\"), 16); print(\",\".join(str(i) for i in range(m.bit_length()) if m >> i & 1))" "$(cat /sys/devices/virtual/workqueue/cpumask)"; }
mark=/work/.bench-check'

# The world as a measurement sees it, a line each. It waits for the command beside it to say
# what it sees meanwhile.
# shellcheck disable=SC2016 # For the commands' shells.
measure="$lib"'
work=${slice%_bench.slice}.slice
echo "cpus $(mine)"
echo "partition $(cat "$slice/cpuset.cpus.partition") $(cat "$slice/cpuset.cpus.effective")"
echo "swap $(cat "$cg/memory.swap.max")"
echo "nice $(ps -o ni= -p $$ | tr -d " ")"
echo "workqueues $(mask)"
echo "work $(cat "$work/cpuset.cpus.effective")"
echo "workio $(grep -c wbps=8000000 "$work/io.max" || true)"
if [[ -d /sys/fs/resctrl/hotbed-bench ]]; then
  echo "l3 $(sed -n "s/^ *L3://p" /sys/fs/resctrl/hotbed-bench/schemata) $(sed -n "s/^ *L3://p" /sys/fs/resctrl/schemata)"
fi
touch "$mark/measuring"
for _ in $(seq 300); do [[ -e $mark/seen ]] && break; sleep 0.1; done
echo "beside $(cat "$mark/seen")"'

# What a command outside measurements sees; with --during, its CPUs once a measurement runs.
# shellcheck disable=SC2016 # For the commands' shells.
look="$lib"'
if [[ ${1:-} == --during ]]; then
  for _ in $(seq 600); do [[ -e $mark/measuring ]] && break; sleep 0.1; done
  mine > "$mark/seen.new" && mv "$mark/seen.new" "$mark/seen"
  exit
fi
echo "cpus $(mine)"
echo "online $(cat /sys/devices/system/cpu/online)"
echo "slice $(cat "$slice/cpuset.cpus.effective")"
echo "workqueues $(mask)"
echo "io $(grep -c wbps=8000000 "$slice/io.max" || true)"
echo "exclusive [$(cat /sys/fs/cgroup/user.slice/cpuset.cpus.exclusive)]"
echo "resctrl $(ls /sys/fs/resctrl 2> /dev/null | grep -c hotbed-bench || true)"
f=/sys/devices/system/cpu/cpu0/cpufreq
if [[ -r $f/scaling_max_freq ]]; then echo "clock0 $(cat $f/scaling_max_freq)"; fi'

run -- bash -c "$look" look > "$tmp/before"
all=$(field online "$tmp/before")
work_cpus=$(cpus minus "$all" "$bench")
check "work runs on every CPU before ($(field cpus "$tmp/before"))" test "$(field cpus "$tmp/before")" == "$(cpus minus "$all" "")"

given_back() { # WHEN: checks the node is as it was before
  run -- bash -c "$look" look > "$tmp/after"
  check "$1: work on every CPU again" test "$(field cpus "$tmp/after")" == "$(field cpus "$tmp/before")"
  check "$1: its slice too" test "$(field slice "$tmp/after")" == "$(field slice "$tmp/before")"
  check "$1: workqueues too" test "$(field workqueues "$tmp/after")" == "$(field workqueues "$tmp/before")"
  check "$1: IO uncapped" test "$(field io "$tmp/after")" == 0
  check "$1: nothing exclusive" test "$(field exclusive "$tmp/after")" == "[]"
  check "$1: no cache class left" test "$(field resctrl "$tmp/after")" == 0
  check "$1: clocks as they were" test "$(field clock0 "$tmp/after")" == "$(field clock0 "$tmp/before")"
}

echo "== a measurement, beside work"
run -- bash -c "rm -rf /work/.bench-check && mkdir /work/.bench-check" > /dev/null
beside=$(runj --detach -- bash -c "$look" look --during | jq -r .data.id)
run --bench -- bash -c "$measure" measure > "$tmp/during"
check "it runs on $bench alone ($(field cpus "$tmp/during"))" test "$(field cpus "$tmp/during")" == "$bench"
check "its slice a partition of them ($(field partition "$tmp/during"))" test "$(field partition "$tmp/during")" == "root $bench"
check "it never swaps (memory.swap.max $(field swap "$tmp/during"))" test "$(field swap "$tmp/during")" == 0
check "at nice $(field nice "$tmp/during")" test "$(field nice "$tmp/during")" == 0
check "unbound workqueues on the rest, $work_cpus ($(field workqueues "$tmp/during"))" \
  test "$(cpus minus "$(field workqueues "$tmp/during")" "")" == "$work_cpus"
check "other work's slice on the rest ($(field work "$tmp/during"))" test "$(field work "$tmp/during")" == "$work_cpus"
check "a command beside it on the rest ($(field beside "$tmp/during"))" test "$(field beside "$tmp/during")" == "$work_cpus"
check "work's IO on the scratch disk capped" test "$(field workio "$tmp/during")" == 1
if grep -q '^l3 ' "$tmp/during"; then
  read -r own others <<< "$(field l3 "$tmp/during")"
  check "a cache class of its own: $own, the others' $others" test "$own" != "$others"
fi
for _ in $(seq 50); do [[ $(state "$beside") == COMMAND_STATE_RUNNING ]] || break; sleep 0.2; done
given_back "after it"

echo "== two measurements"
run -- bash -c "rm -rf /work/.bench-check && mkdir /work/.bench-check" > /dev/null
# shellcheck disable=SC2016 # For the command's shell.
first=$(runj --detach --bench -- bash -c 'for _ in $(seq 300); do [[ -e /work/.bench-check/go ]] && exit; sleep 0.1; done; exit 1' | jq -r .data.id)
for _ in $(seq 100); do [[ $(state "$first") == COMMAND_STATE_RUNNING ]] && break; sleep 0.2; done
second=$(runj --detach --bench -- true | jq -r .data.id)
waits=''
for _ in $(seq 50); do
  waits=$(reason "$second")
  [[ -n $waits ]] && break
  sleep 0.2
done
check "the second waits: $waits" test "$waits" == "waiting for measurement $first to finish"
run -- touch /work/.bench-check/go
for _ in $(seq 100); do [[ $(state "$second") == COMMAND_STATE_SUCCEEDED ]] && break; sleep 0.2; done
check "both succeed, the second after the first" \
  test "$(state "$first") $(state "$second")" == "COMMAND_STATE_SUCCEEDED COMMAND_STATE_SUCCEEDED"
given_back "after them"

echo "== a measurement whose isolation is broken mid-run"
rc=0
# shellcheck disable=SC2016 # For the command's shell.
run --bench -- bash -c "$lib"'
echo "$1" | sudo -n tee "${slice%/*}/cpuset.cpus.exclusive" > /dev/null
sleep 1.5' break "$(cpus droplast "$bench")" > /dev/null 2> "$tmp/broken" || rc=$?
# shellcheck disable=SC2016 # For that shell.
check "it fails, saying it lost its isolation: $(tail -1 "$tmp/broken")" \
  bash -c '[[ $1 == 1 ]] && grep -q ", but the measurement lost its isolation: " "$2"' _ "$rc" "$tmp/broken"
given_back "after it"

echo "== a measurement beside a writer in another session's workspace"
storage=$(hb nodes | jq -r --arg n "$node" '.data.nodes[] | select(.hostname == $n) | .storage')
# shellcheck disable=SC2016 # For the command's shell.
writer=$(hb run -s "$S2" -r "$repo" --node "$node" --no-sync --detach -- bash -c 'rm -f /work/.bench-check-go
while [[ ! -e /work/.bench-check-stop ]]; do
  dd if=/dev/urandom of=/work/.bench-check-write bs=1M count=256 conv=fsync status=none
  touch /work/.bench-check-go
done
rm -f /work/.bench-check-write /work/.bench-check-stop' | jq -r .data.id)
sstate() { hb ps -s "$S2" | jq -r --arg id "$1" '.data.commands[] | select(.id == $id) | .state'; }
for _ in $(seq 300); do [[ $(run2 -- bash -c 'test -e /work/.bench-check-go && echo y || true') == y ]] && break; done
m=$(runj --detach --bench -- sleep 8 | jq -r .data.id)
for _ in $(seq 300); do [[ $(state "$m") == COMMAND_STATE_RUNNING ]] && break; sleep 0.2; done
during=$(sstate "$writer")
for _ in $(seq 100); do [[ $(state "$m") == COMMAND_STATE_RUNNING ]] || break; sleep 0.2; done
check "the measurement holds and succeeds beside the writer ($(state "$m") $(reason "$m"))" test "$(state "$m")" == COMMAND_STATE_SUCCEEDED
if [[ $storage == zfs ]]; then
  check "the writer is held during it ($during)" test "$during" == COMMAND_STATE_PARKED
else
  check "the writer runs on, capped, during it ($during)" test "$during" == COMMAND_STATE_RUNNING
fi
for _ in $(seq 50); do [[ $(sstate "$writer") == COMMAND_STATE_RUNNING ]] && break; sleep 0.2; done
check "the writer goes on after it ($(sstate "$writer"))" test "$(sstate "$writer")" == COMMAND_STATE_RUNNING
run2 -- touch /work/.bench-check-stop > /dev/null
given_back "after them"

echo
if (( failures )); then
  echo "bench-check: $failures failed" >&2
  exit 1
fi
echo "bench-check: all passed"
