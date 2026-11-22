import type { Arch, Repo } from "../types";

const GiB = 1024 ** 3;

export interface NodeSpec {
  id: string;
  hostname: string;
  arch: Arch;
  machine: string;
  kernel: string;
  cpus: number;
  memoryBytes: number;
  swapBytes: number;
  diskBytes: number;
  /** What the OS and daemons hold before any command runs. */
  idleMemoryBytes: number;
  /** Relative speed of one core, so an M2 Max finishes before a T480. */
  speed: number;
  online: boolean;
}

// Old PCs and Apple Silicon laptops on Asahi: the fleet the product is for.
export const NODES: NodeSpec[] = [
  { id: "nUbu1", hostname: "ubu1", arch: "amd64", machine: "Ryzen 7 5800X desktop", kernel: "6.17.4-generic", cpus: 16, memoryBytes: 27 * GiB, swapBytes: 16 * GiB, diskBytes: 1800 * GiB, idleMemoryBytes: 2.6 * GiB, speed: 1.0, online: true },
  { id: "nAsahi", hostname: "asahimbp", arch: "arm64", machine: "MacBook Pro (M3 Pro)", kernel: "7.2.12-402.asahi+16k", cpus: 11, memoryBytes: 18 * GiB, swapBytes: 12 * GiB, diskBytes: 460 * GiB, idleMemoryBytes: 1.7 * GiB, speed: 1.35, online: true },
  { id: "nM2max", hostname: "m2max", arch: "arm64", machine: "MacBook Pro (M2 Max)", kernel: "7.2.12-402.asahi+16k", cpus: 12, memoryBytes: 32 * GiB, swapBytes: 16 * GiB, diskBytes: 920 * GiB, idleMemoryBytes: 1.9 * GiB, speed: 1.25, online: true },
  { id: "nM1pro", hostname: "m1pro", arch: "arm64", machine: "MacBook Pro (M1 Pro)", kernel: "7.2.12-402.asahi+16k", cpus: 10, memoryBytes: 16 * GiB, swapBytes: 12 * GiB, diskBytes: 460 * GiB, idleMemoryBytes: 1.6 * GiB, speed: 1.1, online: true },
  { id: "nM1air", hostname: "m1air", arch: "arm64", machine: "MacBook Air (M1)", kernel: "7.2.12-402.asahi+16k", cpus: 8, memoryBytes: 16 * GiB, swapBytes: 8 * GiB, diskBytes: 230 * GiB, idleMemoryBytes: 1.4 * GiB, speed: 0.95, online: true },
  { id: "nT480", hostname: "t480", arch: "amd64", machine: "ThinkPad T480", kernel: "6.17.4-generic", cpus: 8, memoryBytes: 16 * GiB, swapBytes: 8 * GiB, diskBytes: 480 * GiB, idleMemoryBytes: 1.5 * GiB, speed: 0.55, online: true },
  { id: "nNuc", hostname: "nuc11", arch: "amd64", machine: "Intel NUC 11", kernel: "6.17.4-generic", cpus: 8, memoryBytes: 32 * GiB, swapBytes: 8 * GiB, diskBytes: 920 * GiB, idleMemoryBytes: 1.2 * GiB, speed: 0.7, online: false },
];

export const REPOS: Repo[] = [
  { id: "cladefs", name: "semistrict/cladefs", language: "rust", units: 412, coldBuildSeconds: 580 },
  { id: "ratel", name: "semistrict/ratel", language: "rust", units: 286, coldBuildSeconds: 370 },
  { id: "lnx", name: "semistrict/lnx", language: "rust", units: 197, coldBuildSeconds: 245 },
  { id: "langlang", name: "semistrict/langlang", language: "rust", units: 133, coldBuildSeconds: 175 },
  { id: "hotbed", name: "semistrict/hotbed", language: "go", units: 64, coldBuildSeconds: 70 },
];

/** Which nodes keep a warm base of each repo. */
export const HOSTS: Record<string, string[]> = {
  cladefs: ["nUbu1", "nAsahi", "nM2max"],
  ratel: ["nM2max", "nM1pro", "nUbu1"],
  lnx: ["nAsahi", "nM1air"],
  langlang: ["nM1pro", "nT480"],
  hotbed: ["nUbu1", "nM1air", "nT480"],
};

export const BASE_SIZE: Record<string, number> = {
  cladefs: 38 * GiB,
  ratel: 24 * GiB,
  lnx: 17 * GiB,
  langlang: 9 * GiB,
  hotbed: 2.1 * GiB,
};

export const AGENTS = ["claude", "codex", "claude", "claude", "pi", "codex"];

/** The machines agents run hotbed from: a session's agent. */
export const AGENT_HOSTS = ["ramons-macbook-pro", "ramons-macbook-pro", "ramons-macbook-pro", "mbp-air", "ubu1"];

/** Session names are adjective-noun, as hotbed session start picks them. */
export const ADJECTIVES = ["brave", "calm", "eager", "fond", "gentle", "keen", "live", "mellow", "noble", "poetic", "quiet", "rapid", "steady", "tidy", "vivid", "witty", "bold", "clever", "humble", "lucid"];
export const NOUNS = ["otter", "newt", "walleye", "ox", "gnu", "yak", "owl", "heron", "lynx", "badger", "marten", "ibis", "tapir", "finch", "hare", "koi", "mole", "seal", "wren", "elk"];

export const TOPICS: Record<string, string[]> = {
  cladefs: ["dax-fault-path", "extent-gc", "fork-time", "graft-store", "pmem-flush", "overlay-whiteouts", "lsm-compaction"],
  ratel: ["raft-leases", "range-split", "node-local-uuids", "txn-pipelining", "snapshot-ship"],
  lnx: ["fork-memory", "vsock-reconnect", "virtiofs-dax", "idle-snapshot"],
  langlang: ["peg-memo", "error-recovery", "incremental-lexer"],
  hotbed: ["btrfs-forks", "cgroup-freeze", "admission-psi", "ui-cockpit"],
};

export const COMMIT_MESSAGES = [
  "Batch extent frees in one transaction",
  "Retry lease renewal on a stale epoch",
  "Keep the overlay's whiteouts out of the index",
  "Flush pmem before acking a fork",
  "Split ranges at the median key, not the midpoint",
  "Reuse the vsock connection across restores",
  "Memoize PEG rules at the choice point",
  "Thaw parked commands oldest first",
  "Drop the global lock from the compaction path",
  "Move fingerprint checks off the hot path",
  "Teach the store about grafted parents",
  "Bound the snapshot queue",
];

export interface CommandTemplate {
  argv: string;
  kind: "build" | "test" | "check" | "server" | "shell";
  /** Typical crates rebuilt; 0 for commands that build nothing. */
  rebuilt: [number, number];
  /** Seconds of work after the build: running tests, say. */
  run: [number, number];
  memoryGiB: [number, number];
  cores: [number, number];
  failRate: number;
}

export const COMMANDS: Record<"rust" | "go" | "typescript", CommandTemplate[]> = {
  rust: [
    { argv: "cargo build", kind: "build", rebuilt: [1, 9], run: [0, 0], memoryGiB: [2.5, 7], cores: [5, 12], failRate: 0.08 },
    { argv: "cargo nextest run", kind: "test", rebuilt: [2, 14], run: [6, 40], memoryGiB: [3, 9], cores: [4, 12], failRate: 0.16 },
    { argv: "cargo nextest run -p {crate}", kind: "test", rebuilt: [1, 5], run: [2, 12], memoryGiB: [1, 3], cores: [3, 8], failRate: 0.12 },
    { argv: "cargo clippy --all-targets", kind: "check", rebuilt: [1, 10], run: [0, 0], memoryGiB: [2, 5], cores: [4, 10], failRate: 0.1 },
    { argv: "cargo check", kind: "check", rebuilt: [1, 6], run: [0, 0], memoryGiB: [0.8, 2.5], cores: [3, 8], failRate: 0.06 },
  ],
  go: [
    { argv: "go test ./...", kind: "test", rebuilt: [1, 6], run: [3, 15], memoryGiB: [0.6, 1.6], cores: [2, 8], failRate: 0.1 },
    { argv: "go build ./...", kind: "build", rebuilt: [1, 5], run: [0, 0], memoryGiB: [0.4, 1.2], cores: [2, 8], failRate: 0.04 },
    { argv: "pnpm -C ui dev --host", kind: "server", rebuilt: [0, 0], run: [0, 0], memoryGiB: [0.4, 0.8], cores: [0.05, 0.3], failRate: 0 },
  ],
  typescript: [],
};

export const CRATES: Record<string, string[]> = {
  cladefs: ["cladefs-store", "cladefs-fxfs", "cladefs-dax", "cladefs-fuse", "cladefs-gc", "cladefs-cli"],
  ratel: ["ratel-raft", "ratel-kv", "ratel-sql", "ratel-txn", "ratel-server"],
  lnx: ["lnx-vmm", "lnx-init", "lnx-vsock", "lnx-snapshot"],
  langlang: ["langlang-peg", "langlang-lexer", "langlang-cli"],
  hotbed: ["internal/controller", "internal/worker", "internal/api"],
};
