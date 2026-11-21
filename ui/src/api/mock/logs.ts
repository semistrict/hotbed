import type { Command, FinishedCommand } from "../types";
import { CRATES } from "./fixtures";
import { Rng } from "./rng";

function seedOf(id: string): number {
  let h = 2166136261;
  for (let i = 0; i < id.length; i++) h = Math.imul(h ^ id.charCodeAt(i), 16777619);
  return h >>> 0;
}

interface LogInput {
  id: string;
  repoId: string;
  argv: string;
  kind: Command["kind"];
  state: Command["state"];
  rebuilt: number;
  seconds: number;
  failure: FinishedCommand["failure"];
  url: string | null;
}

/** Output a command of this shape would print, the same every time for one ID. */
export function mockLog(c: LogInput): string[] {
  const rng = new Rng(seedOf(c.id));
  const crates = CRATES[c.repoId] ?? ["core"];
  const root = `/work/${c.repoId}`;
  const lines: string[] = [];
  if (c.kind === "server") {
    lines.push("", "  VITE v8.3.8  ready in 412 ms", "", `  ➜  Network: ${c.url ?? "http://0.0.0.0:5173"}/`, "");
    let t = rng.int(9, 14) * 3600 + rng.int(0, 3599);
    for (let i = 0; i < rng.int(3, 9); i++) {
      t += rng.int(20, 600);
      lines.push(`${clockOf(t)} [vite] hmr update /src/${rng.pick(["App", "Nodes", "Chart", "Fork"])}.tsx`);
    }
    return lines;
  }
  if (c.argv.startsWith("go ")) {
    for (const p of crates) lines.push(`ok  \tgithub.com/semistrict/hotbed/${p}\t${rng.range(0.2, 3).toFixed(3)}s`);
    if (c.failure) lines.push("--- FAIL: TestAdmitUnderPressure (0.02s)", "    admit_test.go:88: admitted with PSI 31%, want queued", "FAIL");
    return lines;
  }
  for (let i = 0; i < c.rebuilt; i++) {
    const crate = i < crates.length ? crates[crates.length - 1 - i] : `${rng.pick(crates)}-${rng.pick(["macros", "proto", "test-util"])}`;
    lines.push(`   Compiling ${crate} v0.${rng.int(1, 9)}.0 (${root}/crates/${crate})`);
  }
  if (c.failure === "compile") {
    const crate = rng.pick(crates);
    lines.push(
      `error[E0308]: mismatched types`,
      `   --> crates/${crate}/src/lib.rs:${rng.int(40, 400)}:${rng.int(5, 30)}`,
      "    |",
      `${String(rng.int(40, 400)).padStart(3)} |     let epoch: u64 = lease.epoch();`,
      "    |                ---   ^^^^^^^^^^^^^ expected `u64`, found `Epoch`",
      "",
      `error: could not compile \`${crate}\` (lib) due to 1 previous error`,
    );
    return lines;
  }
  if (c.failure === "lint") {
    lines.push(
      "error: this `if` has identical blocks",
      `   --> crates/${rng.pick(crates)}/src/gc.rs:${rng.int(10, 200)}:9`,
      "    = note: `-D clippy::if-same-then-else` implied by `-D warnings`",
      "error: could not compile due to 1 previous error",
    );
    return lines;
  }
  const build = Math.max(1, c.seconds * rng.range(0.3, 0.7));
  lines.push(`    Finished \`test\` profile [unoptimized + debuginfo] target(s) in ${build.toFixed(2)}s`);
  if (c.kind !== "test") return lines;
  const tests = rng.int(40, 400);
  lines.push("────────────", ` Nextest run ID ${rng.hex(8)}-${rng.hex(4)} with nextest profile: default`, `    Starting ${tests} tests across ${crates.length} binaries`);
  const failing = c.failure === "test" ? rng.int(1, 3) : 0;
  for (let i = 0; i < Math.min(14, tests); i++) {
    const crate = rng.pick(crates);
    const name = `${rng.pick(["lease", "split", "gc", "fork", "snapshot", "txn"])}::${rng.pick(["renews_before_expiry", "survives_restart", "rejects_stale_epoch", "frees_extents", "keeps_order"])}`;
    const fail = i < failing;
    lines.push(`        ${fail ? "FAIL" : "PASS"} [   ${rng.range(0.001, 2).toFixed(3)}s] ${crate} ${name}`);
    if (fail) lines.push("", `--- STDERR: ${crate} ${name} ---`, `thread '${name}' panicked at crates/${crate}/src/${name.split("::")[0]}.rs:${rng.int(20, 300)}:5:`, "assertion `left == right` failed", "  left: 41", " right: 42", "");
  }
  if (tests > 14) lines.push(`        ... ${tests - 14} more`);
  lines.push("────────────", `     Summary [${c.seconds.toFixed(3)}s] ${tests} tests run: ${tests - failing} passed, ${failing} failed, ${rng.int(0, 4)} skipped`);
  if (c.state === "running") lines.pop();
  return lines;
}

function clockOf(seconds: number): string {
  const p = (n: number) => String(Math.floor(n)).padStart(2, "0");
  return `${p((seconds / 3600) % 24)}:${p((seconds / 60) % 60)}:${p(seconds % 60)}`;
}
