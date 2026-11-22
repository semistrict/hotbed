import type {
  CommandProfile,
  FoldedStack,
  InspectCommandResult,
  InspectSection,
  ProfileBucket,
  ProfileFunction,
  ProfileQuery,
  TraceChunk,
  TraceOptions,
  TraceSummary,
} from "../types";

export function mockInspect(id: string, argv: string, opts?: { sections?: InspectSection[]; pid?: number }): InspectCommandResult {
  const want = (s: InspectSection) => !opts?.sections?.length || opts.sections.includes(s);
  const isRust = argv.includes("cargo") || argv.includes("rustc");
  const mainComm = isRust ? "cargo" : "go";
  const childComm = isRust ? "rustc" : "compile";
  const mainPid = 41204;
  const childPid = 41289;
  const childPid2 = 41290;

  const procs = want("procs")
    ? [
        {
          pid: mainPid,
          ppid: 1,
          state: "S",
          comm: mainComm,
          cmdline: argv || `${mainComm} test ./...`,
          threadsCount: 5,
          rssBytes: 68 * 1024 ** 2,
          vsizeBytes: 1_240 * 1024 ** 2,
          cpuUserMs: 4120,
          cpuSysMs: 920,
          ioReadBytes: 42 * 1024 ** 2,
          ioWriteBytes: 11 * 1024 ** 2,
          wchan: "ep_poll",
          oomScore: 12,
          voluntaryCtxtSwitches: 1842,
          nonvoluntaryCtxtSwitches: 145,
          threads: [
            { tid: mainPid, name: mainComm, state: "S", wchan: "ep_poll", cpuUserMs: 2100, cpuSysMs: 500 },
            { tid: mainPid + 1, name: `${mainComm}-worker`, state: "S", wchan: "futex_wait_queue", cpuUserMs: 1420, cpuSysMs: 310 },
            { tid: mainPid + 2, name: `${mainComm}-io`, state: "S", wchan: "pipe_read", cpuUserMs: 600, cpuSysMs: 110 },
          ],
        },
        {
          pid: childPid,
          ppid: mainPid,
          state: "R",
          comm: childComm,
          cmdline: isRust ? "rustc --crate-name hotbed_core --edition=2024 src/lib.rs -C opt-level=2" : "/usr/local/go/pkg/tool/linux_amd64/compile -o pkg.a -race ./...",
          threadsCount: 8,
          rssBytes: 420 * 1024 ** 2,
          vsizeBytes: 1_850 * 1024 ** 2,
          cpuUserMs: 18950,
          cpuSysMs: 1840,
          ioReadBytes: 128 * 1024 ** 2,
          ioWriteBytes: 64 * 1024 ** 2,
          wchan: "",
          oomScore: 84,
          voluntaryCtxtSwitches: 6421,
          nonvoluntaryCtxtSwitches: 932,
          threads: [
            { tid: childPid, name: childComm, state: "R", wchan: "", cpuUserMs: 9400, cpuSysMs: 780 },
            { tid: childPid + 1, name: "codegen-0", state: "R", wchan: "", cpuUserMs: 5200, cpuSysMs: 520 },
            { tid: childPid + 2, name: "codegen-1", state: "S", wchan: "futex_wait_queue", cpuUserMs: 4350, cpuSysMs: 540 },
          ],
        },
        {
          pid: childPid2,
          ppid: mainPid,
          state: "S",
          comm: "test-server",
          cmdline: "./target/debug/test-server --listen 127.0.0.1:18080",
          threadsCount: 4,
          rssBytes: 44 * 1024 ** 2,
          vsizeBytes: 760 * 1024 ** 2,
          cpuUserMs: 880,
          cpuSysMs: 210,
          ioReadBytes: 4 * 1024 ** 2,
          ioWriteBytes: 2 * 1024 ** 2,
          wchan: "ep_poll",
          oomScore: 8,
          voluntaryCtxtSwitches: 940,
          nonvoluntaryCtxtSwitches: 34,
          threads: [],
        },
      ]
    : [];

  const allFiles = [
    { pid: mainPid, comm: mainComm, fd: 0, kind: "device" as const, target: "/dev/null", mode: "r", pos: 0, inode: 0, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: mainPid, comm: mainComm, fd: 1, kind: "file" as const, target: `/var/lib/hotbed/worker/logs/${id}.log`, mode: "w", pos: 18432, inode: 0, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: mainPid, comm: mainComm, fd: 3, kind: "anon_inode" as const, target: "anon_inode:[eventpoll]", mode: "rw", pos: 0, inode: 0, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: mainPid, comm: mainComm, fd: 5, kind: "pipe" as const, target: "pipe:[884120]", mode: "r", pos: 0, inode: 884120, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: childPid, comm: childComm, fd: 1, kind: "pipe" as const, target: "pipe:[884120]", mode: "w", pos: 0, inode: 884120, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: childPid, comm: childComm, fd: 7, kind: "file" as const, target: "/work/hotbed/internal/worker/commands.go", mode: "r", pos: 4096, inode: 0, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: childPid, comm: childComm, fd: 8, kind: "file" as const, target: "/work/.hotbed/cache/go-build/a4/a4f8d2c1", mode: "rw", pos: 262144, inode: 0, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: childPid, comm: childComm, fd: 9, kind: "deleted" as const, target: "/tmp/hotbed-cmd/build-scratch.tmp (deleted)", mode: "rw", pos: 1048576, inode: 0, socketProto: "", socketState: "", localAddr: "", remoteAddr: "" },
    { pid: childPid2, comm: "test-server", fd: 6, kind: "socket" as const, target: "socket:[912044]", mode: "rw", pos: 0, inode: 912044, socketProto: "tcp", socketState: "LISTEN", localAddr: "127.0.0.1:18080", remoteAddr: "0.0.0.0:0" },
    { pid: childPid2, comm: "test-server", fd: 11, kind: "socket" as const, target: "socket:[912089]", mode: "rw", pos: 0, inode: 912089, socketProto: "tcp", socketState: "ESTABLISHED", localAddr: "127.0.0.1:18080", remoteAddr: "127.0.0.1:54312" },
    { pid: childPid2, comm: "test-server", fd: 12, kind: "socket" as const, target: "socket:[912101]", mode: "rw", pos: 0, inode: 912101, socketProto: "unix", socketState: "CONNECTED", localAddr: "/run/hotbed/test.sock", remoteAddr: "" },
  ];

  const pidFilter = opts?.pid ?? 0;
  const matchPid = <T extends { pid: number }>(xs: T[]) => (pidFilter > 0 ? xs.filter((x) => x.pid === pidFilter) : xs);

  return {
    commandId: id,
    collectedAt: Date.now(),
    cgroup: `/hotbed.slice/hotbed-${id}.service`,
    unitName: `hotbed-${id}`,
    procs,
    files: want("files") ? matchPid(allFiles) : [],
    cgroupStats: want("cgroup")
      ? {
          cpuUsageUsec: 26_920_000,
          cpuUserUsec: 23_950_000,
          cpuSystemUsec: 2_970_000,
          nrThrottled: 3,
          throttledUsec: 14_200,
          memoryCurrentBytes: 532 * 1024 ** 2,
          memoryPeakBytes: 690 * 1024 ** 2,
          memoryAnonBytes: 382 * 1024 ** 2,
          memoryFileBytes: 112 * 1024 ** 2,
          memoryKernelBytes: 24 * 1024 ** 2,
          memorySlabBytes: 14 * 1024 ** 2,
          memorySockBytes: 2 * 1024 ** 2,
          memoryShmemBytes: 4 * 1024 ** 2,
          memorySwapBytes: 0,
          pgfault: 184_290,
          pgmajfault: 14,
          pidsCurrent: 3,
          pidsPeak: 11,
          psiCpuSomeAvg10: 1.84,
          psiCpuFullAvg10: 0.12,
          psiMemSomeAvg10: 0.0,
          psiMemFullAvg10: 0.0,
          psiIoSomeAvg10: 0.45,
          psiIoFullAvg10: 0.08,
          io: [{ device: "259:0", rbytes: 174 * 1024 ** 2, wbytes: 77 * 1024 ** 2, rios: 3420, wios: 1890 }],
        }
      : null,
    kstacks: want("kstack")
      ? matchPid([
          {
            pid: mainPid,
            tid: mainPid,
            comm: mainComm,
            threadName: mainComm,
            state: "S",
            wchan: "ep_poll",
            frames: ["do_epoll_wait+0x94/0xe0", "__x64_sys_epoll_pwait+0x70/0x110", "do_syscall_64+0x5c/0x90", "entry_SYSCALL_64_after_hwframe+0x76/0x7e"],
          },
          {
            pid: mainPid,
            tid: mainPid + 1,
            comm: mainComm,
            threadName: `${mainComm}-worker`,
            state: "S",
            wchan: "futex_wait_queue",
            frames: ["futex_wait_queue+0x60/0xa0", "futex_wait+0x158/0x250", "do_futex+0x120/0x190", "__x64_sys_futex+0x7a/0x1c0", "do_syscall_64+0x5c/0x90"],
          },
          {
            pid: childPid,
            tid: childPid + 2,
            comm: childComm,
            threadName: "codegen-1",
            state: "S",
            wchan: "futex_wait_queue",
            frames: ["futex_wait_queue+0x60/0xa0", "futex_wait+0x158/0x250", "do_futex+0x120/0x190", "__x64_sys_futex+0x7a/0x1c0", "do_syscall_64+0x5c/0x90"],
          },
        ])
      : [],
    maps: want("maps")
      ? matchPid([
          {
            pid: childPid,
            comm: childComm,
            rssBytes: 420 * 1024 ** 2,
            pssBytes: 398 * 1024 ** 2,
            privateDirtyBytes: 365 * 1024 ** 2,
            sharedCleanBytes: 48 * 1024 ** 2,
            swapBytes: 0,
            anonBytes: 360 * 1024 ** 2,
            shmemBytes: 4 * 1024 ** 2,
            topRegions: [
              { perms: "rw-p", path: "[anon]", sizeBytes: 480 * 1024 ** 2, rssBytes: 0, pssBytes: 0 },
              { perms: "r-xp", path: isRust ? "/work/.hotbed/rustup/toolchains/stable/lib/librustc_driver.so" : "/usr/local/go/pkg/tool/linux_amd64/compile", sizeBytes: 142 * 1024 ** 2, rssBytes: 0, pssBytes: 0 },
              { perms: "r--p", path: "/usr/lib/x86_64-linux-gnu/libc.so.6", sizeBytes: 2 * 1024 ** 2, rssBytes: 0, pssBytes: 0 },
            ],
          },
        ])
      : [],
    limits: want("limits")
      ? matchPid([
          {
            pid: mainPid,
            comm: mainComm,
            capEff: "0000000000000000",
            seccomp: "disabled",
            noNewPrivs: false,
            items: [
              { name: "Max open files", soft: "1048576", hard: "1048576", units: "files" },
              { name: "Max processes", soft: "102184", hard: "102184", units: "processes" },
              { name: "Max locked memory", soft: "8388608", hard: "8388608", units: "bytes" },
              { name: "Max stack size", soft: "8388608", hard: "unlimited", units: "bytes" },
            ],
          },
        ])
      : [],
    availableTools: [
      { name: "strace", available: true, detail: "/usr/bin/strace" },
      { name: "perf", available: true, detail: "/usr/bin/perf" },
      { name: "eu-stack", available: true, detail: "/usr/bin/eu-stack" },
      { name: "gdb", available: true, detail: "/usr/bin/gdb" },
      { name: "bpftrace", available: true, detail: "/usr/bin/bpftrace" },
    ],
  };
}

export async function mockTrace(
  id: string,
  opts: TraceOptions,
  onChunk: (chunk: TraceChunk) => void,
  signal?: AbortSignal,
): Promise<TraceSummary | null> {
  const dur = Math.min(Math.max(opts.durationSeconds ?? 5, 1), 60);
  const startedAt = Date.now();
  const sleep = (ms: number) =>
    new Promise<void>((resolve, reject) => {
      const t = setTimeout(resolve, ms);
      signal?.addEventListener("abort", () => {
        clearTimeout(t);
        reject(new DOMException("Aborted", "AbortError"));
      });
    });

  onChunk({ status: `attaching ${opts.kind} to hotbed-${id}.service (${dur}s)...`, text: "", summary: null, done: false });
  await sleep(220);

  if (opts.kind === "strace") {
    const lines = [
      `41289 openat(AT_FDCWD, "/work/hotbed/internal/worker/commands.go", O_RDONLY|O_CLOEXEC) = 7 <0.000031>\n`,
      `41289 fstat(7</work/hotbed/internal/worker/commands.go>, {st_mode=S_IFREG|0644, st_size=24112}) = 0 <0.000014>\n`,
      `41289 read(7</work/hotbed/internal/worker/commands.go>, "package worker\\n\\nimport (\\n...", 32768) = 24112 <0.000048>\n`,
      `41289 close(7</work/hotbed/internal/worker/commands.go>) = 0 <0.000011>\n`,
      `41290 epoll_pwait(3<anon_inode:[eventpoll]>, [{events=EPOLLIN, data={u32=11}}], 128, 1000, NULL, 8) = 1 <0.002140>\n`,
      `41290 read(11<TCP:[127.0.0.1:18080->127.0.0.1:54312]>, "POST /hotbed.v1.WorkspaceService/GetCommand HTTP/2\\r\\n", 4096) = 164 <0.000022>\n`,
      `41289 futex(0xc000102148, FUTEX_WAKE_PRIVATE, 1) = 1 <0.000018>\n`,
    ];
    for (const l of lines) {
      await sleep(110);
      onChunk({ status: "", text: l, summary: null, done: false });
    }
    const summary: TraceSummary = {
      kind: opts.kind,
      startedAt,
      endedAt: Date.now(),
      durationMs: Date.now() - startedAt,
      pidsAttached: [41204, 41289, 41290],
      commandLine: `strace -f -T -yy -s 120${opts.filter ? ` -e trace=${opts.filter}` : ""} -p 41204 -p 41289 -p 41290`,
      stacks: [],
      topFunctions: [],
      counters: [],
      syscalls: [],
      raw: "",
    };
    onChunk({ status: "", text: "", summary, done: true });
    return summary;
  }

  await sleep(280);
  const endedAt = Date.now();
  let summary: TraceSummary;
  switch (opts.kind) {
    case "strace_summary":
      summary = {
        kind: opts.kind,
        startedAt,
        endedAt,
        durationMs: endedAt - startedAt,
        pidsAttached: [41204, 41289, 41290],
        commandLine: `strace -f -c${opts.filter ? ` -e trace=${opts.filter}` : ""} -p 41204 -p 41289 -p 41290`,
        stacks: [],
        topFunctions: [],
        counters: [],
        syscalls: [
          { name: "futex", pctTime: 44.2, seconds: 0.842, usecsPerCall: 52, calls: 16190, errors: 1420 },
          { name: "epoll_pwait", pctTime: 28.1, seconds: 0.535, usecsPerCall: 412, calls: 1298, errors: 0 },
          { name: "read", pctTime: 12.6, seconds: 0.24, usecsPerCall: 18, calls: 13310, errors: 12 },
          { name: "write", pctTime: 8.9, seconds: 0.17, usecsPerCall: 21, calls: 8095, errors: 0 },
          { name: "openat", pctTime: 4.1, seconds: 0.078, usecsPerCall: 26, calls: 3004, errors: 184 },
          { name: "newfstatat", pctTime: 2.1, seconds: 0.04, usecsPerCall: 9, calls: 4412, errors: 62 },
        ],
        raw: "% time     seconds  usecs/call     calls    errors syscall\n------ ----------- ----------- --------- --------- ----------------\n 44.20    0.842000          52     16190      1420 futex\n 28.10    0.535000         412      1298           epoll_pwait\n 12.60    0.240000          18     13310        12 read\n  8.90    0.170000          21      8095           write\n  4.10    0.078000          26      3004       184 openat\n  2.10    0.040000           9      4412        62 newfstatat\n------ ----------- ----------- --------- --------- ----------------\n100.00    1.905000          41     46309      1678 total\n",
      };
      break;
    case "perf_cpu": {
      const stacks: FoldedStack[] = [
        { stack: "compile;cmd/compile/internal/ssagen.Compile;cmd/compile/internal/ssa.Compile;cmd/compile/internal/ssa.regalloc", samples: 168, leafType: "go", comm: "compile" },
        { stack: "compile;cmd/compile/internal/ssagen.Compile;cmd/compile/internal/ssa.Compile;cmd/compile/internal/ssa.opt", samples: 112, leafType: "go", comm: "compile" },
        { stack: "compile;cmd/compile/internal/types2.Check;cmd/compile/internal/types2.(*Checker).exprInternal", samples: 84, leafType: "go", comm: "compile" },
        { stack: "compile;runtime.mallocgc;runtime.gcBgMarkWorker;runtime.scanobject", samples: 62, leafType: "go", comm: "compile" },
        { stack: "compile;syscall.Syscall;entry_SYSCALL_64_[k];do_syscall_64_[k];vfs_write_[k];zfs_write_[k]", samples: 41, leafType: "kernel", comm: "compile" },
        { stack: "go;os/exec.(*Cmd).Wait;syscall.wait4;entry_SYSCALL_64_[k];do_wait_[k]", samples: 19, leafType: "kernel", comm: "go" },
      ];
      const topFunctions: ProfileFunction[] = [
        { name: "cmd/compile/internal/ssa.regalloc", module: "compile", frameType: "go", sourceFile: "", sourceLine: 0, selfSamples: 168, totalSamples: 168, selfPct: 34.57, totalPct: 34.57 },
        { name: "cmd/compile/internal/ssa.opt", module: "compile", frameType: "go", sourceFile: "", sourceLine: 0, selfSamples: 112, totalSamples: 112, selfPct: 23.05, totalPct: 23.05 },
        { name: "cmd/compile/internal/types2.(*Checker).exprInternal", module: "compile", frameType: "go", sourceFile: "", sourceLine: 0, selfSamples: 84, totalSamples: 84, selfPct: 17.28, totalPct: 17.28 },
        { name: "runtime.scanobject", module: "compile", frameType: "go", sourceFile: "", sourceLine: 0, selfSamples: 62, totalSamples: 62, selfPct: 12.76, totalPct: 12.76 },
        { name: "zfs_write_[k]", module: "zfs", frameType: "kernel", sourceFile: "", sourceLine: 0, selfSamples: 41, totalSamples: 41, selfPct: 8.44, totalPct: 8.44 },
        { name: "do_wait_[k]", module: "vmlinux", frameType: "kernel", sourceFile: "", sourceLine: 0, selfSamples: 19, totalSamples: 19, selfPct: 3.91, totalPct: 3.91 },
      ];
      summary = {
        kind: opts.kind,
        startedAt,
        endedAt,
        durationMs: endedAt - startedAt,
        pidsAttached: [41204, 41289, 41290],
        commandLine: `perf record -F ${opts.frequencyHz ?? 99} -g --call-graph dwarf,16384 -p 41204,41289,41290 -- sleep ${dur}`,
        stacks,
        topFunctions,
        counters: [],
        syscalls: [],
        raw: " 34.57%  compile  compile       [.] cmd/compile/internal/ssa.regalloc\n 23.05%  compile  compile       [.] cmd/compile/internal/ssa.opt\n 17.28%  compile  compile       [.] cmd/compile/internal/types2.(*Checker).exprInternal\n 12.76%  compile  compile       [.] runtime.scanobject\n  8.44%  compile  [zfs]         [k] zfs_write\n  3.91%  go       [kernel]      [k] do_wait\n",
      };
      break;
    }
    case "perf_stat":
      summary = {
        kind: opts.kind,
        startedAt,
        endedAt,
        durationMs: endedAt - startedAt,
        pidsAttached: [41204, 41289, 41290],
        commandLine: `perf stat -e task-clock,context-switches,cpu-migrations,page-faults,cycles,instructions,branches,branch-misses,cache-references,cache-misses -p 41204,41289,41290 -- sleep ${dur}`,
        stacks: [],
        topFunctions: [],
        counters: [
          { name: "task-clock", value: "14,820.41", unit: "msec", note: "2.964 CPUs utilized" },
          { name: "context-switches", value: "18,412", unit: "", note: "1.242 K/sec" },
          { name: "cpu-migrations", value: "412", unit: "", note: "27.80 /sec" },
          { name: "page-faults", value: "42,918", unit: "", note: "2.896 K/sec" },
          { name: "cycles", value: "61,420,918,220", unit: "", note: "4.144 GHz" },
          { name: "instructions", value: "118,240,112,840", unit: "", note: "1.93 insn per cycle" },
          { name: "branches", value: "24,180,440,120", unit: "", note: "1.632 G/sec" },
          { name: "branch-misses", value: "418,220,104", unit: "", note: "1.73% of all branches" },
          { name: "cache-references", value: "1,420,118,000", unit: "", note: "95.82 M/sec" },
          { name: "cache-misses", value: "118,402,110", unit: "", note: "8.34% of all cache refs" },
        ],
        syscalls: [],
        raw: " Performance counter stats for process id '41204,41289,41290':\n\n         14,820.41 msec task-clock                #    2.964 CPUs utilized\n            18,412      context-switches          #    1.242 K/sec\n               412      cpu-migrations            #   27.800 /sec\n            42,918      page-faults               #    2.896 K/sec\n    61,420,918,220      cycles                    #    4.144 GHz\n   118,240,112,840      instructions              #    1.93  insn per cycle\n    24,180,440,120      branches                  #    1.632 G/sec\n       418,220,104      branch-misses             #    1.73% of all branches\n     1,420,118,000      cache-references          #   95.822 M/sec\n       118,402,110      cache-misses              #    8.338% of all cache refs\n",
      };
      break;
    case "stacks":
      summary = {
        kind: opts.kind,
        startedAt,
        endedAt,
        durationMs: endedAt - startedAt,
        pidsAttached: [41204, 41289],
        commandLine: "eu-stack -p 41204,41289",
        stacks: [],
        topFunctions: [],
        counters: [],
        syscalls: [],
        raw: "=== PID 41289 (compile) ===\nTID 41289:\n#0  0x00000000007a4b20 cmd/compile/internal/ssa.regalloc\n#1  0x0000000000791044 cmd/compile/internal/ssa.Compile\n#2  0x000000000081c2e0 cmd/compile/internal/ssagen.Compile\nTID 41291:\n#0  0x000000000046f810 runtime.futex\n#1  0x000000000043b224 runtime.notesleep\n--- Kernel stacks ---\nTID 41291 (codegen-1) [S] wchan=futex_wait_queue\n  futex_wait_queue+0x60/0xa0\n  futex_wait+0x158/0x250\n  do_syscall_64+0x5c/0x90\n",
      };
      break;
    default: {
      const rawByKind: Record<string, string> = {
        bpf_execsnoop: `41289    compile          /usr/local/go/pkg/tool/linux_amd64/compile -o /work/.hotbed/cache/b001.a ./internal/worker\n41312    link             /usr/local/go/pkg/tool/linux_amd64/link -o /tmp/go-build/worker.test\n41340    worker.test      /tmp/go-build/worker.test -test.v -test.race\n`,
        bpf_opensnoop: `41289    compile          /work/hotbed/internal/worker/commands.go\n41289    compile          /work/hotbed/internal/worker/diagnostics.go\n41340    worker.test      /tmp/hotbed-test-9214/worker.db\n`,
        bpf_vfsstat: `@[kprobe:vfs_read, compile]: 4821\n@[kprobe:vfs_write, compile]: 1940\n@[kprobe:vfs_open, compile]: 812\n@[kprobe:vfs_fsync, worker.test]: 64\n`,
        bpf_offcpu: `@[\n    finish_task_switch.isra.0+142\n    __schedule+840\n    schedule+92\n    futex_wait_queue+96\n, compile]: 418200\n@[\n    finish_task_switch.isra.0+142\n    __schedule+840\n    schedule+92\n    do_epoll_wait+148\n, test-server]: 1940200\n`,
        bpf_net: `@[tcp_sendmsg, test-server]: 142\n@[tcp_recvmsg, test-server]: 138\n@[tcp_v4_connect, worker.test]: 12\n`,
      };
      const raw = rawByKind[opts.kind] ?? "";
      if (opts.kind === "bpf_execsnoop" || opts.kind === "bpf_opensnoop") {
        onChunk({ status: "", text: raw, summary: null, done: false });
      }
      summary = {
        kind: opts.kind,
        startedAt,
        endedAt,
        durationMs: endedAt - startedAt,
        pidsAttached: [41204, 41289, 41290],
        commandLine: `bpftrace -e '/* cgroup == cgroupid("/sys/fs/cgroup/hotbed.slice/hotbed-${id}.service") */'`,
        stacks: [],
        topFunctions: [],
        counters: [],
        syscalls: [],
        raw,
      };
    }
  }
  onChunk({ status: "", text: "", summary, done: true });
  return summary;
}

export function mockCommandProfile(id: string, argv: string, live: boolean, startedAt: number, endedAt: number | null, query?: ProfileQuery): CommandProfile {
  const isRust = argv.includes("cargo") || argv.includes("rustc");
  const now = endedAt ?? Date.now();
  const baseStacks: FoldedStack[] = isRust
    ? [
        { stack: "rustc;rustc_driver_impl::run_compiler;rustc_interface::passes::start_codegen;rustc_codegen_llvm::codegen_crate;llvm::PassManager::run", samples: 312, leafType: "native", comm: "rustc" },
        { stack: "rustc;rustc_driver_impl::run_compiler;rustc_hir_analysis::check_crate;rustc_hir_typeck::typeck", samples: 184, leafType: "native", comm: "rustc" },
        { stack: "rustc;rustc_driver_impl::run_compiler;rustc_borrowck::mir_borrowck;rustc_borrowck::region_inference::solve", samples: 118, leafType: "native", comm: "rustc" },
        { stack: "rustc;rustc_driver_impl::run_compiler;rustc_metadata::rmeta::decoder::decode_metadata", samples: 74, leafType: "native", comm: "rustc" },
        { stack: "rustc;std::fs::write;entry_SYSCALL_64_[k];do_syscall_64_[k];vfs_write_[k];zfs_write_[k]", samples: 58, leafType: "kernel", comm: "rustc" },
        { stack: "cargo;cargo::ops::cargo_compile::compile;entry_SYSCALL_64_[k];do_syscall_64_[k];do_epoll_wait_[k]", samples: 34, leafType: "kernel", comm: "cargo" },
        { stack: "lld;lld::elf::link;lld::elf::Writer::writeSections;memcpy_erms", samples: 92, leafType: "native", comm: "lld" },
      ]
    : [
        { stack: "compile;cmd/compile/internal/ssagen.Compile;cmd/compile/internal/ssa.Compile;cmd/compile/internal/ssa.regalloc", samples: 245, leafType: "go", comm: "compile" },
        { stack: "compile;cmd/compile/internal/ssagen.Compile;cmd/compile/internal/ssa.Compile;cmd/compile/internal/ssa.opt", samples: 162, leafType: "go", comm: "compile" },
        { stack: "compile;cmd/compile/internal/types2.Check;cmd/compile/internal/types2.(*Checker).exprInternal", samples: 124, leafType: "go", comm: "compile" },
        { stack: "worker.test;testing.tRunner;github.com/semistrict/hotbed/internal/worker.TestEngine;runtime.racefuncenter", samples: 138, leafType: "go", comm: "worker.test" },
        { stack: "compile;runtime.gcBgMarkWorker;runtime.scanobject", samples: 88, leafType: "go", comm: "compile" },
        { stack: "compile;syscall.Syscall;entry_SYSCALL_64_[k];do_syscall_64_[k];vfs_write_[k];zfs_write_[k]", samples: 64, leafType: "kernel", comm: "compile" },
        { stack: "worker.test;modernc.org/sqlite.(*conn).exec;syscall.Syscall;entry_SYSCALL_64_[k];vfs_fsync_[k]", samples: 47, leafType: "kernel", comm: "worker.test" },
      ];

  const q = (query?.query ?? "").trim().toLowerCase();
  const ft = (query?.frameType ?? "").trim().toLowerCase();
  const comm = (query?.comm ?? "").trim();

  const totalSamples = baseStacks.reduce((a, b) => a + b.samples, 0);
  const filtered = baseStacks.filter((st) => {
    if (comm && st.comm !== comm) return false;
    if (ft && st.leafType.toLowerCase() !== ft) return false;
    if (q && !st.stack.toLowerCase().includes(q) && !st.comm.toLowerCase().includes(q)) return false;
    return true;
  });

  const matchedSamples = filtered.reduce((a, b) => a + b.samples, 0);
  const kernelSamples = filtered.filter((s) => s.leafType === "kernel").reduce((a, b) => a + b.samples, 0);
  const userSamples = matchedSamples - kernelSamples;

  const fnMap = new Map<string, { self: number; total: number; ft: string; mod: string }>();
  for (const st of filtered) {
    const parts = st.stack.split(";");
    const seen = new Set<string>();
    parts.forEach((name, idx) => {
      const isLeaf = idx === parts.length - 1;
      const frameType = name.endsWith("_[k]") ? "kernel" : isLeaf ? st.leafType : isRust ? "native" : "go";
      const cur = fnMap.get(name) ?? { self: 0, total: 0, ft: frameType, mod: st.comm };
      if (!seen.has(name)) {
        cur.total += st.samples;
        seen.add(name);
      }
      if (isLeaf) cur.self += st.samples;
      fnMap.set(name, cur);
    });
  }

  const topFunctions: ProfileFunction[] = Array.from(fnMap.entries())
    .map(([name, v]) => ({
      name,
      module: v.mod,
      frameType: v.ft,
      sourceFile: "",
      sourceLine: 0,
      selfSamples: v.self,
      totalSamples: v.total,
      selfPct: matchedSamples > 0 ? (100 * v.self) / matchedSamples : 0,
      totalPct: matchedSamples > 0 ? (100 * v.total) / matchedSamples : 0,
    }))
    .sort((a, b) => b.selfSamples - a.selfSamples || b.totalSamples - a.totalSamples);

  // Build 5-second timeline buckets over the command's duration.
  const bucketMs = 5000;
  const span = Math.max(bucketMs * 6, now - startedAt);
  const nBuckets = Math.min(24, Math.max(6, Math.floor(span / bucketMs)));
  const startT = Math.floor((now - nBuckets * bucketMs) / bucketMs) * bucketMs;
  const buckets: ProfileBucket[] = [];
  for (let i = 0; i < nBuckets; i++) {
    const t = startT + i * bucketMs;
    if (query?.fromMs && t + bucketMs < query.fromMs) continue;
    if (query?.toMs && t > query.toMs) continue;
    const wave = 0.65 + 0.55 * Math.sin(i * 0.9 + 0.5);
    const s = Math.max(4, Math.round((totalSamples / nBuckets) * wave));
    const k = Math.max(1, Math.round(s * (0.12 + 0.08 * Math.cos(i))));
    buckets.push({
      t,
      samples: s,
      kernelSamples: k,
      userSamples: s - k,
      topFunction: topFunctions[i % Math.max(1, Math.min(3, topFunctions.length))]?.name ?? "",
    });
  }

  return {
    commandId: id,
    profiler: "opentelemetry-ebpf-profiler",
    live,
    active: true,
    statusNote: "",
    sampleHz: 20,
    startedAt,
    updatedAt: now,
    totalSamples,
    matchedSamples,
    kernelSamples,
    userSamples,
    comms: Array.from(new Set(baseStacks.map((s) => s.comm))).sort(),
    frameTypes: Array.from(new Set(baseStacks.map((s) => s.leafType))).sort(),
    stacks: filtered,
    topFunctions,
    buckets,
  };
}
