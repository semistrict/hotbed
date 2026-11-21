import type { FileContent, FileEntry, Workspace } from "../types";
import { CRATES } from "./fixtures";
import { Rng } from "./rng";

function seedOf(id: string): number {
  let h = 2166136261;
  for (let i = 0; i < id.length; i++) h = Math.imul(h ^ id.charCodeAt(i), 16777619);
  return h >>> 0;
}

const RUST_MODULES = ["lib", "lease", "split", "gc", "store", "fork", "snapshot", "error"];
const GO_PACKAGES = ["controller", "worker", "api", "statedir", "tailnet"];

function paths(repoId: string): string[] {
  if (repoId === "hotbed") {
    const out = ["go.mod", "go.sum", "README.md", "cmd/hotbed/main.go", "cmd/hotbed/commands.go", "ui/package.json", "ui/src/main.tsx"];
    for (const p of GO_PACKAGES) out.push(`internal/${p}/${p}.go`, `internal/${p}/${p}_test.go`);
    return out;
  }
  const out = ["Cargo.toml", "Cargo.lock", "rust-toolchain.toml", "README.md", ".config/nextest.toml"];
  for (const crate of CRATES[repoId] ?? []) {
    out.push(`crates/${crate}/Cargo.toml`);
    for (const m of RUST_MODULES.slice(0, 3 + (crate.length % 5))) out.push(`crates/${crate}/src/${m}.rs`);
    out.push(`crates/${crate}/tests/${crate.replace(/-/g, "_")}.rs`);
  }
  return out;
}

/** The workspace's tree, with what it changed against its base. */
export function mockFiles(w: Workspace): FileEntry[] {
  const rng = new Rng(seedOf(w.id));
  const files = paths(w.repoId);
  const changed = new Map<string, FileEntry["change"]>();
  const sources = files.filter((f) => f.endsWith(".rs") || f.endsWith(".go") || f.endsWith(".tsx"));
  for (let i = 0; i < Math.min(w.filesChanged, sources.length); i++) changed.set(rng.pick(sources), rng.chance(0.2) ? "added" : "modified");
  const dirs = new Set<string>();
  for (const f of files) {
    const parts = f.split("/");
    for (let i = 1; i < parts.length; i++) dirs.add(parts.slice(0, i).join("/"));
  }
  const entries: FileEntry[] = [...dirs].map((d) => ({ path: d, kind: "dir", sizeBytes: 0, change: null }));
  for (const f of files) entries.push({ path: f, kind: "file", sizeBytes: f.endsWith(".lock") ? 182_000 : rng.int(600, 24_000), change: changed.get(f) ?? null });
  return entries.sort((a, b) => a.path.localeCompare(b.path));
}

export function mockFile(w: Workspace, path: string): FileContent {
  const rng = new Rng(seedOf(w.id + path));
  const entry = mockFiles(w).find((e) => e.path === path);
  const name = path.split("/").pop() ?? path;
  let text: string;
  if (name === "Cargo.toml") {
    text = `[package]\nname = "${path.split("/")[1] ?? w.repoId}"\nversion = "0.4.0"\nedition = "2024"\n\n[dependencies]\nanyhow = "1"\nbytes = "1"\ntokio = { version = "1", features = ["rt-multi-thread", "macros"] }\ntracing = "0.1"\n`;
  } else if (name.endsWith(".rs")) {
    const m = name.replace(".rs", "");
    text = [
      `//! ${m}: part of ${w.repoId}.`,
      "",
      "use std::time::{Duration, Instant};",
      "",
      "use anyhow::{bail, Result};",
      "",
      `/// How long a ${m} holds before it must renew.`,
      "const TTL: Duration = Duration::from_secs(9);",
      "",
      "#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]",
      "pub struct Epoch(pub u64);",
      "",
      `pub struct ${cap(m)} {`,
      "    epoch: Epoch,",
      "    expires: Instant,",
      "}",
      "",
      `impl ${cap(m)} {`,
      "    pub fn new(epoch: Epoch, now: Instant) -> Self {",
      "        Self { epoch, expires: now + TTL }",
      "    }",
      "",
      "    /// Extends the lease, refusing an epoch older than the one held.",
      "    pub fn renew(&mut self, epoch: Epoch, now: Instant) -> Result<()> {",
      "        if epoch < self.epoch {",
      '            bail!("stale epoch {:?} < {:?}", epoch, self.epoch);',
      "        }",
      "        self.epoch = epoch;",
      "        self.expires = now + TTL;",
      "        Ok(())",
      "    }",
      "",
      "    pub fn valid(&self, now: Instant) -> bool {",
      "        now < self.expires",
      "    }",
      "}",
    ].join("\n");
  } else if (name.endsWith(".go")) {
    text = `package ${path.split("/")[1] ?? "main"}\n\nimport (\n\t"context"\n\t"log/slog"\n)\n\n// Admit decides whether a command fits on this node now.\nfunc Admit(ctx context.Context, n Node, c Command) (bool, string) {\n\tif n.ReservedLow+c.MemoryLow > n.Memory*86/100 {\n\t\treturn false, "waiting for memory"\n\t}\n\tif n.MemoryPressure > 22 {\n\t\tslog.InfoContext(ctx, "queued under pressure", "psi", n.MemoryPressure)\n\t\treturn false, "waiting: memory pressure"\n\t}\n\treturn true, ""\n}\n`;
  } else {
    text = `# ${w.repoId}\n\nWorkspace ${w.id}, forked for ${w.agent}.\n`;
  }
  const n = text.split("\n").length;
  const changedLines: number[] = [];
  if (entry?.change === "added") for (let i = 1; i <= n; i++) changedLines.push(i);
  else if (entry?.change === "modified") {
    const start = rng.int(5, Math.max(6, n - 6));
    for (let i = start; i < Math.min(n, start + rng.int(2, 7)); i++) changedLines.push(i);
  }
  return { path, text, changedLines };
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
