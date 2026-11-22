import type {
  Base,
  ClusterEvent,
  Command,
  EventKind,
  FailureKind,
  FinishedCommand,
  HourBucket,
  LoadSample,
  Node,
  NodeHour,
  Repo,
  Sample,
  Session,
  Snapshot,
  Workspace,
} from "../types";
import {
  ADJECTIVES,
  AGENT_HOSTS,
  AGENTS,
  BASE_SIZE,
  COMMANDS,
  COMMIT_MESSAGES,
  CRATES,
  HOSTS,
  NODES,
  NOUNS,
  REPOS,
  TOPICS,
  type CommandTemplate,
  type NodeSpec,
} from "./fixtures";
import { Rng } from "./rng";

const GiB = 1024 ** 3;
const HOUR = 3600_000;
const SAMPLE_SECONDS = 5;
const LIVE_SAMPLES = 180; // 15 minutes
const WARMUP_SECONDS = 20 * 60;
const KEEP_ENDED_MS = 120_000;
const TARGET_WORKSPACES = 22;
const MAX_EVENTS = 80;
/** Bases build this many times faster than real, so a demo sees one finish. */
const BASE_BUILD_SPEEDUP = 8;

/** What the simulation tracks about a live command beyond what the API shows. */
interface Run {
  template: CommandTemplate;
  workLeft: number; // core-seconds of compiling at speed 1
  workTotal: number;
  runLeft: number; // seconds of running after the build
  coresWant: number;
  memoryTarget: number;
  willFail: FailureKind | null;
  parks: number;
  parkedAt: number;
  peakMemory: number;
  /** Crates the build will compile. */
  units: number;
}

/**
 * A cluster that runs in the browser: agents fork workspaces from warm bases,
 * run builds, tests and dev servers in them, the nodes admit, share and park
 * that work, and main moves on so bases rebuild. It starts from a fixed seed
 * and runs from twenty minutes before the hour up to now, so the first render
 * already looks lived in.
 */
export class MockCluster {
  private rng: Rng;
  private now: number;
  private nextSample: number;
  private eventId = 0;

  private specs = new Map<string, NodeSpec>();
  private nodeUsage = new Map<string, Node["usage"]>();
  private bases: Base[] = [];
  private workspaces: Workspace[] = [];
  private commands: Command[] = [];
  private runs = new Map<string, Run>();
  private history: FinishedCommand[] = [];
  /** Live sessions and those that ended in the last day; the synthesized day's are in past. */
  private sessions: Session[] = [];
  private past = new Map<string, Session>();
  private names = new Set<string>();
  private events: ClusterEvent[] = [];
  private series: Record<string, Sample[]> = {};
  private load: LoadSample[] = [];
  private hourly: HourBucket[] = [];
  private nodeHourly: Record<string, NodeHour[]> = {};
  private main = new Map<string, { generation: number; commit: string; message: string; nextAt: number }>();
  private forkMillis: number[] = [];
  /** Core-seconds each node ran since its last sample, so a sample is the mean, not an instant. */
  private coreSeconds = new Map<string, number>();

  constructor(seed: number, now: number) {
    this.rng = new Rng(seed);
    // Start from the top of the hour, not from now: the simulation is
    // deterministic, so every page load replays the same cluster and a link
    // to a workspace or command still works after a reload.
    const start = Math.floor(now / HOUR) * HOUR - WARMUP_SECONDS * 1000;
    this.now = start;
    this.nextSample = start;
    for (const spec of NODES) {
      this.specs.set(spec.id, spec);
      this.nodeUsage.set(spec.id, { cpuBusy: 0, memoryUsedBytes: spec.idleMemoryBytes, swapUsedBytes: 0, cpuPressure: 0, memoryPressure: 0 });
      this.series[spec.id] = [];
    }
    this.synthesizeHistory(start);
    this.seedBases();
    while (this.workspaces.length < TARGET_WORKSPACES - 3) this.fork(true);
    while (this.now < now) this.step(1);
  }

  /** Runs the cluster forward to wall-clock time t, one second at a time. */
  advanceTo(t: number): void {
    while (this.now + 1000 <= t) this.step(1);
  }

  snapshot(): Snapshot {
    const nodes: Node[] = NODES.map((spec) => this.node(spec));
    const snap: Snapshot = structuredClone({
      clusterId: "54074bcb94bbfbe2",
      controller: "ubu1",
      now: iso(this.now),
      sampleSeconds: SAMPLE_SECONDS,
      nodes,
      repos: REPOS,
      bases: this.bases,
      workspaces: this.workspaces,
      commands: this.commands,
      events: this.events,
      totals24h: this.totals(),
      series: this.series,
      load: this.load,
      hourly: this.hourly,
      nodeHourly: this.nodeHourly,
      history: [] as FinishedCommand[],
      sessions: [...this.sessions, ...this.past.values()].sort((a, b) => b.lastActiveAt.localeCompare(a.lastActiveAt)),
    }) satisfies Snapshot;
    // Finished commands never change once recorded, so they need no copy.
    snap.history = [...this.history];
    return snap;
  }

  findCommand(id: string): { command: Command | null; finished: FinishedCommand | null } {
    return {
      command: this.commands.find((c) => c.id === id) ?? null,
      finished: this.history.find((h) => h.id === id) ?? null,
    };
  }

  findWorkspace(id: string): Workspace | null {
    return this.workspaces.find((w) => w.id === id) ?? null;
  }

  // ---- one second of the cluster -------------------------------------

  private step(dt: number): void {
    this.now += dt * 1000;
    this.advanceMain();
    this.buildBases(dt);
    this.agentsAct(dt);
    for (const spec of NODES) {
      if (!spec.online) continue;
      this.admit(spec);
      this.share(spec, dt);
      this.parkOrThaw(spec);
    }
    this.retire();
    if (this.now >= this.nextSample) {
      this.sample();
      this.nextSample += SAMPLE_SECONDS * 1000;
    }
  }

  private advanceMain(): void {
    for (const repo of REPOS) {
      const m = this.main.get(repo.id);
      if (!m || this.now < m.nextAt) continue;
      m.generation += 1;
      m.commit = this.rng.hex(7);
      m.message = this.rng.pick(COMMIT_MESSAGES);
      m.nextAt = this.now + this.rng.range(4, 11) * 60_000;
      for (const nodeId of HOSTS[repo.id] ?? []) {
        if (!this.specs.get(nodeId)?.online) continue;
        this.bases.push(this.newBase(repo, nodeId, m.generation, m.commit, m.message, "building"));
      }
    }
  }

  private buildBases(dt: number): void {
    for (const b of this.bases) {
      if (b.state !== "building") continue;
      const repo = repoOf(b.repoId);
      const spec = this.spec(b.nodeId);
      const seconds = repo.coldBuildSeconds / spec.speed;
      b.progress = Math.min(1, b.progress + (dt * BASE_BUILD_SPEEDUP) / seconds);
      if (b.progress < 1) continue;
      b.state = "warm";
      b.builtAt = iso(this.now);
      b.buildSeconds = Math.round(seconds * this.rng.range(0.95, 1.08));
      for (const old of this.bases) {
        if (old !== b && old.repoId === b.repoId && old.nodeId === b.nodeId && old.state === "warm") old.state = "stale";
      }
      this.event("base", b.nodeId, `base ${repo.id} g${b.generation} warm on ${spec.hostname} · built in ${clock(b.buildSeconds)} · ${gib(b.sizeBytes)}`, null);
    }
    // A stale base goes once no workspace forked from it is left.
    this.bases = this.bases.filter((b) => b.state !== "stale" || this.workspaces.some((w) => w.baseId === b.id && w.state !== "released"));
  }

  private agentsAct(dt: number): void {
    const live = this.workspaces.filter((w) => w.state !== "released");
    if (live.length < TARGET_WORKSPACES && this.rng.chance(dt / 25)) this.fork(false);
    for (const w of live) {
      const busy = this.commands.some((c) => c.workspaceId === w.id && isLive(c) && c.kind !== "server");
      const repo = repoOf(w.repoId);
      if (!busy && this.rng.chance((dt / 25) * diurnal(new Date(this.now).getHours()))) {
        const templates = COMMANDS[repo.language].filter((t) => t.kind !== "server");
        this.submit(w, this.rng.pick(templates));
      }
      const hasServer = this.commands.some((c) => c.workspaceId === w.id && c.kind === "server" && isLive(c));
      const server = COMMANDS[repo.language].find((t) => t.kind === "server");
      if (server && !hasServer && this.rng.chance(dt / 400)) this.submit(w, server);
      if (!busy && !hasServer && Date.parse(w.lastActiveAt) < this.now - 150_000 && this.rng.chance(dt / 90)) {
        w.state = "released";
        this.event("release", w.nodeId, `release ${w.id} · ${w.agent} · ${gib(w.exclusiveBytes)} freed`, { type: "workspace", id: w.id });
        this.released(w);
      }
    }
  }

  /** A workspace's session ends with its last workspace: by its agent, or now and then abandoned. */
  private released(w: Workspace): void {
    const s = this.sessions.find((x) => x.id === w.sessionId);
    if (!s) return;
    const sw = s.workspaces.find((x) => x.id === w.id);
    if (sw) sw.releasedAt = iso(this.now);
    if (s.workspaces.some((x) => x.releasedAt === null)) return;
    s.endedAt = iso(this.now);
    s.endReason = this.rng.chance(0.12) ? "abandoned: nothing touched it for an hour" : "ended by its agent";
  }

  private sessionName(): string {
    for (;;) {
      const name = `${this.rng.pick(ADJECTIVES)}-${this.rng.pick(NOUNS)}`;
      if (!this.names.has(name)) {
        this.names.add(name);
        return name;
      }
    }
  }

  private submit(w: Workspace, template: CommandTemplate): void {
    const repo = repoOf(w.repoId);
    const spec = this.spec(w.nodeId);
    const crate = this.rng.pick(CRATES[repo.id] ?? ["core"]);
    const argv = template.argv.replace("{crate}", crate);
    const rebuilt = template.kind === "server" ? 0 : this.rng.int(template.rebuilt[0], template.rebuilt[1]);
    const perUnit = repo.coldBuildSeconds / repo.units;
    const link = repo.language === "rust" ? this.rng.range(3, 8) : this.rng.range(1, 3);
    const buildSeconds = template.kind === "server" ? 2 : 1.5 + rebuilt * perUnit * this.rng.range(1.2, 2.2) + link;
    const coresWant = Math.min(spec.cpus, this.rng.range(template.cores[0], template.cores[1]));
    const memoryTarget = this.rng.range(template.memoryGiB[0], template.memoryGiB[1]) * GiB;
    const willFail: FailureKind | null = this.rng.chance(template.failRate)
      ? template.kind === "test"
        ? this.rng.chance(0.8) ? "test" : "compile"
        : template.kind === "check" && argv.includes("clippy")
          ? "lint"
          : "compile"
      : null;
    const id = `c-${this.rng.hex(6)}`;
    const session = this.sessions.find((x) => x.id === w.sessionId);
    if (session) session.lastActiveAt = iso(this.now);
    const cmd: Command = {
      id,
      sessionId: w.sessionId,
      workspaceId: w.id,
      nodeId: w.nodeId,
      argv,
      kind: template.kind,
      state: "queued",
      queuedAt: iso(this.now),
      startedAt: null,
      endedAt: null,
      exitCode: null,
      limits: {
        cpuWeight: template.kind === "server" ? 20 : 100,
        memoryLowBytes: roundGiB(memoryTarget * 0.6),
        memoryHighBytes: roundGiB(memoryTarget * 1.6),
      },
      cpuCores: 0,
      memoryBytes: 0,
      swapBytes: 0,
      rebuilt: template.kind === "server" ? null : { units: 0, of: repo.units },
      coldSeconds: template.kind === "server" ? null : Math.round(repo.coldBuildSeconds / spec.speed + 0),
      reason: null,
      url: template.kind === "server" ? `http://${spec.hostname}:${5170 + this.rng.int(0, 29)}` : null,
      timeline: [{ at: iso(this.now), what: "queued", detail: `by ${w.agent}` }],
    };
    const runLeft = template.kind === "server" ? Infinity : this.rng.range(template.run[0], template.run[1]);
    if (cmd.coldSeconds !== null) cmd.coldSeconds = Math.round(cmd.coldSeconds + runLeft);
    this.runs.set(id, {
      template,
      workLeft: buildSeconds * coresWant,
      workTotal: buildSeconds * coresWant,
      runLeft,
      coresWant,
      memoryTarget,
      willFail,
      parks: 0,
      parkedAt: 0,
      peakMemory: 0,
      units: rebuilt,
    });
    this.commands.push(cmd);
    w.lastActiveAt = iso(this.now);
    w.state = "active";
  }

  private admit(spec: NodeSpec): void {
    const usage = this.usage(spec.id);
    for (const c of this.commands) {
      if (c.state !== "queued" || c.nodeId !== spec.id) continue;
      const reserved = this.reserved(spec.id);
      const room = spec.memoryBytes * 0.86 - reserved;
      if (c.limits.memoryLowBytes > room) {
        c.reason = `waiting for memory: needs ${gib(c.limits.memoryLowBytes)} guaranteed, ${gib(Math.max(0, room))} left on ${spec.hostname}`;
        continue;
      }
      if (usage.memoryPressure > 22) {
        c.reason = `waiting: memory pressure ${usage.memoryPressure.toFixed(0)}% on ${spec.hostname}`;
        continue;
      }
      c.state = "running";
      c.startedAt = iso(this.now);
      c.reason = null;
      c.timeline.push({ at: iso(this.now), what: "admitted", detail: `weight ${c.limits.cpuWeight} · low ${gib(c.limits.memoryLowBytes)} · high ${gib(c.limits.memoryHighBytes)}` });
      const waited = (this.now - Date.parse(c.queuedAt)) / 1000;
      this.event("admit", spec.id, `admit ${c.argv} · ${c.workspaceId}${waited > 2 ? ` · waited ${clock(waited)}` : ""}`, { type: "command", id: c.id });
    }
  }

  /** Splits the node's CPUs between running commands by weight, and moves each along. */
  private share(spec: NodeSpec, dt: number): void {
    const running = this.commands.filter((c) => c.nodeId === spec.id && c.state === "running");
    let want = 0;
    for (const c of running) want += this.run(c.id).coresWant * (c.limits.cpuWeight / 100);
    const share = want > spec.cpus ? spec.cpus / want : 1;
    for (const c of running) {
      const r = this.run(c.id);
      const compiling = r.workLeft > 0;
      const cores = compiling ? r.coresWant * (c.limits.cpuWeight / 100) * share : c.kind === "server" ? r.coresWant : Math.min(3, r.coresWant) * share;
      c.cpuCores = cores * this.rng.range(0.9, 1.05);
      this.coreSeconds.set(spec.id, (this.coreSeconds.get(spec.id) ?? 0) + c.cpuCores * dt);
      c.memoryBytes += (r.memoryTarget - c.memoryBytes) * (1 - Math.exp(-dt / 5));
      r.peakMemory = Math.max(r.peakMemory, c.memoryBytes);
      if (compiling) {
        r.workLeft -= cores * spec.speed * dt;
        if (c.rebuilt) c.rebuilt.units = Math.round(r.units * (1 - Math.max(0, r.workLeft) / r.workTotal));
        if (r.workLeft <= 0 && r.willFail === "compile") this.finish(c, "failed");
      } else {
        r.runLeft -= dt;
        if (r.runLeft <= 0) this.finish(c, r.willFail ? "failed" : "succeeded");
      }
    }
  }

  private parkOrThaw(spec: NodeSpec): void {
    const used = this.memoryUsed(spec);
    if (used > spec.memoryBytes * 0.93) {
      const victim = this.commands
        .filter((c) => c.nodeId === spec.id && c.state === "running" && c.kind !== "server")
        .sort((a, b) => Date.parse(b.startedAt ?? "") - Date.parse(a.startedAt ?? ""))[0];
      if (victim) {
        const r = this.run(victim.id);
        victim.state = "parked";
        victim.swapBytes = victim.memoryBytes * 0.85;
        victim.memoryBytes *= 0.15;
        victim.cpuCores = 0;
        victim.reason = `frozen at memory.high on ${spec.hostname}; ${gib(victim.swapBytes)} moved to swap`;
        victim.timeline.push({ at: iso(this.now), what: "parked", detail: victim.reason });
        r.parks += 1;
        r.parkedAt = this.now;
        this.bumpHour((h) => (h.parks += 1));
        this.event("park", spec.id, `park ${victim.argv} on ${spec.hostname} · memory.high · ${gib(victim.swapBytes)} → swap`, { type: "command", id: victim.id });
      }
      return;
    }
    const parked = this.commands
      .filter((c) => c.nodeId === spec.id && c.state === "parked")
      .sort((a, b) => this.run(a.id).parkedAt - this.run(b.id).parkedAt)[0];
    if (parked && used + parked.swapBytes < spec.memoryBytes * 0.78) {
      const r = this.run(parked.id);
      const slept = (this.now - r.parkedAt) / 1000;
      parked.state = "running";
      parked.memoryBytes += parked.swapBytes;
      parked.swapBytes = 0;
      parked.reason = null;
      parked.timeline.push({ at: iso(this.now), what: "thawed", detail: `after ${clock(slept)}` });
      this.event("thaw", spec.id, `thaw ${parked.argv} on ${spec.hostname} after ${clock(slept)}`, { type: "command", id: parked.id });
    }
  }

  private finish(c: Command, state: "succeeded" | "failed"): void {
    const r = this.run(c.id);
    const w = this.findWorkspace(c.workspaceId);
    const repo = repoOf(w?.repoId ?? "hotbed");
    c.state = state;
    c.endedAt = iso(this.now);
    c.exitCode = state === "succeeded" ? 0 : r.willFail === "test" ? 100 : 101;
    c.cpuCores = 0;
    c.timeline.push({ at: iso(this.now), what: "exited", detail: `exit ${c.exitCode}` });
    const seconds = (this.now - Date.parse(c.startedAt ?? c.queuedAt)) / 1000;
    const failure = state === "failed" ? r.willFail : null;
    if (w) {
      w.lastActiveAt = iso(this.now);
      w.exclusiveBytes += ((c.rebuilt?.units ?? 0) * (BASE_SIZE[repo.id] ?? GiB) * this.rng.range(0.04, 0.15)) / repo.units;
      w.filesChanged += this.rng.chance(0.3) ? 1 : 0;
      w.state = "idle";
    }
    const record: FinishedCommand = {
      id: c.id,
      sessionId: c.sessionId,
      repoId: repo.id,
      nodeId: c.nodeId,
      workspaceId: c.workspaceId,
      agent: w?.agent ?? "claude",
      argv: c.argv,
      kind: c.kind,
      state,
      exitCode: c.exitCode,
      failure,
      endedAt: this.now,
      seconds,
      coldSeconds: c.coldSeconds,
      rebuilt: c.rebuilt?.units ?? null,
      of: c.rebuilt?.of ?? null,
      peakMemoryBytes: r.peakMemory,
      parks: r.parks,
    };
    this.history.push(record);
    this.bumpHour((h) => {
      h.builds += 1;
      h.spentSeconds += seconds;
      if (c.coldSeconds !== null) h.savedSeconds += Math.max(0, c.coldSeconds - seconds);
    });
    const units = c.rebuilt ? ` · ${c.rebuilt.units}/${c.rebuilt.of} crates` : "";
    if (state === "succeeded") {
      const speedup = c.coldSeconds ? ` (cold ${clock(c.coldSeconds)}, ${Math.round(c.coldSeconds / seconds)}×)` : "";
      this.event("finish", c.nodeId, `ok ${c.argv} · ${repo.id}${units} · ${clock(seconds)}${speedup}`, { type: "command", id: c.id });
    } else {
      this.event("fail", c.nodeId, `fail ${c.argv} · ${repo.id} · exit ${c.exitCode} · ${failureText(failure)}`, { type: "command", id: c.id });
    }
  }

  private retire(): void {
    this.commands = this.commands.filter((c) => {
      if (isLive(c)) {
        const w = this.findWorkspace(c.workspaceId);
        if (w?.state === "released") {
          c.state = "cancelled";
          c.endedAt = iso(this.now);
          c.timeline.push({ at: iso(this.now), what: "cancelled", detail: "workspace released" });
        }
        return true;
      }
      const keep = Date.parse(c.endedAt ?? "") > this.now - KEEP_ENDED_MS;
      if (!keep) this.runs.delete(c.id);
      return keep;
    });
    this.workspaces = this.workspaces.filter((w) => w.state !== "released" || Date.parse(w.lastActiveAt) > this.now - 600_000);
    this.sessions = this.sessions.filter((x) => x.endedAt === null || Date.parse(x.endedAt) > this.now - 24 * HOUR);
    const day = this.now - 24 * HOUR;
    while (this.history[0] && this.history[0].endedAt < day) this.history.shift();
  }

  private sample(): void {
    for (const spec of NODES) {
      const s = this.series[spec.id];
      if (!s) continue;
      const usage = this.usage(spec.id);
      if (!spec.online) {
        usage.cpuBusy = 0;
        continue;
      }
      const cores = (this.coreSeconds.get(spec.id) ?? 0) / SAMPLE_SECONDS;
      this.coreSeconds.set(spec.id, 0);
      let swap = 0;
      for (const c of this.commands) if (c.nodeId === spec.id && c.state === "parked") swap += c.swapBytes;
      const used = this.memoryUsed(spec);
      const frac = used / spec.memoryBytes;
      usage.cpuBusy = clamp(cores / spec.cpus + this.rng.range(0.01, 0.05), 0, 1);
      usage.memoryUsedBytes = used;
      usage.swapUsedBytes = swap;
      usage.memoryPressure = Math.max(0, (frac - 0.72) * 140 + (swap > 0 ? 6 : 0) + this.rng.range(-1, 1));
      usage.cpuPressure = Math.max(0, (usage.cpuBusy - 0.8) * 90 + this.rng.range(0, 2));
      s.push({ t: this.now, cpuBusy: usage.cpuBusy, memoryUsed: frac, memoryPressure: usage.memoryPressure });
      if (s.length > LIVE_SAMPLES) s.shift();
      const hours = this.nodeHourly[spec.id];
      const last = hours?.[hours.length - 1];
      if (last && last.hour === hourOf(this.now)) {
        last.cpuBusy = last.cpuBusy * 0.98 + usage.cpuBusy * 0.02;
        last.memoryUsed = last.memoryUsed * 0.98 + frac * 0.02;
      }
    }
    let running = 0;
    let queued = 0;
    let parked = 0;
    for (const c of this.commands) {
      if (c.state === "running") running++;
      else if (c.state === "queued") queued++;
      else if (c.state === "parked") parked++;
    }
    this.load.push({ t: this.now, running, queued, parked });
    if (this.load.length > LIVE_SAMPLES) this.load.shift();
  }

  // ---- workspaces and bases ---------------------------------------------

  private fork(initial: boolean): void {
    const repo = this.rng.pick(REPOS);
    const hosts = (HOSTS[repo.id] ?? []).filter((id) => this.specs.get(id)?.online);
    const nodeId = hosts.sort((a, b) => this.freeMemory(b) - this.freeMemory(a))[this.rng.chance(0.7) ? 0 : hosts.length - 1];
    if (!nodeId) return;
    const base = this.bases
      .filter((b) => b.repoId === repo.id && b.nodeId === nodeId && b.state !== "building")
      .sort((a, b) => b.generation - a.generation)[0];
    if (!base) return;
    const agent = this.rng.pick(AGENTS);
    const topic = this.rng.pick(TOPICS[repo.id] ?? ["work"]);
    const ms = Math.round(this.rng.logNormal(31, 0.35));
    this.forkMillis.push(ms);
    if (this.forkMillis.length > 200) this.forkMillis.shift();
    const createdAt = initial ? this.now - this.rng.range(60, 3600) * 1000 : this.now;
    // Now and then a live session adds a workspace of another repo; mostly an
    // agent starts a session for the repo it works on.
    const others = this.sessions.filter((x) => x.endedAt === null && !x.workspaces.some((sw) => sw.repoId === repo.id));
    let session = !initial && others.length > 0 && this.rng.chance(0.2) ? this.rng.pick(others) : undefined;
    if (!session) {
      session = { id: this.sessionName(), agent: this.rng.pick(AGENT_HOSTS), startedAt: iso(createdAt), lastActiveAt: iso(createdAt), endedAt: null, endReason: "", workspaces: [] };
      this.sessions.push(session);
    }
    const w: Workspace = {
      id: `ws-${this.rng.hex(4)}`,
      sessionId: session.id,
      repoId: repo.id,
      baseId: base.id,
      nodeId,
      agent: `${agent}/${topic}`,
      branch: `${agent}/${topic}`,
      head: this.rng.hex(7),
      filesChanged: initial ? this.rng.int(1, 14) : 0,
      state: "idle",
      reason: "",
      createdAt: iso(createdAt),
      forkMillis: ms,
      exclusiveBytes: (initial ? this.rng.range(0.15, 2.2) : 0.01) * GiB,
      sharedBytes: base.sizeBytes,
      lastActiveAt: iso(createdAt),
    };
    this.workspaces.push(w);
    session.workspaces.push({ id: w.id, repoId: repo.id, nodeId, createdAt: w.createdAt, releasedAt: null });
    session.lastActiveAt = iso(Math.max(Date.parse(session.lastActiveAt), createdAt));
    this.bumpHour((h) => (h.forks += 1));
    if (!initial) {
      this.event("fork", nodeId, `fork ${w.id} ← ${repo.id}@g${base.generation} on ${this.spec(nodeId).hostname} in ${ms} ms · ${w.agent}`, { type: "workspace", id: w.id });
    }
  }

  private seedBases(): void {
    for (const repo of REPOS) {
      const generation = this.rng.int(30, 140);
      const commit = this.rng.hex(7);
      const message = this.rng.pick(COMMIT_MESSAGES);
      this.main.set(repo.id, { generation, commit, message, nextAt: this.now + this.rng.range(1, 9) * 60_000 });
      for (const nodeId of HOSTS[repo.id] ?? []) {
        const spec = this.spec(nodeId);
        const stale = this.newBase(repo, nodeId, generation - 1, this.rng.hex(7), this.rng.pick(COMMIT_MESSAGES), "stale");
        stale.builtAt = iso(this.now - this.rng.range(2, 5) * HOUR);
        const warm = this.newBase(repo, nodeId, generation, commit, message, "warm");
        warm.builtAt = iso(this.now - this.rng.range(5, 90) * 60_000);
        if (spec.online) this.bases.push(stale, warm);
        else this.bases.push({ ...warm, builtAt: iso(this.now - 3.2 * HOUR) });
      }
    }
  }

  private newBase(repo: Repo, nodeId: string, generation: number, commit: string, message: string, state: Base["state"]): Base {
    const spec = this.spec(nodeId);
    const baked =
      repo.language === "rust"
        ? ["cargo build --all-targets", "cargo nextest run --no-run", "cargo clippy --all-targets"]
        : ["go build ./...", "go test -run ^$ ./..."];
    return {
      id: `b-${repo.id}-${spec.hostname}-g${generation}`,
      repoId: repo.id,
      nodeId,
      generation,
      commit,
      message,
      state,
      progress: state === "building" ? 0 : 1,
      builtAt: state === "building" ? null : iso(this.now),
      buildSeconds: Math.round((repo.coldBuildSeconds / spec.speed) * this.rng.range(0.95, 1.08)),
      sizeBytes: (BASE_SIZE[repo.id] ?? 5 * GiB) * this.rng.range(0.96, 1.04),
      toolchain: repo.language === "rust" ? "rustc 1.94.1" : "go 1.27.4",
      baked,
    };
  }

  // ---- 24 hours of history -------------------------------------------

  /** Writes the day before the warm-up: finished commands and hourly buckets that agree with them. */
  private synthesizeHistory(start: number): void {
    const firstHour = hourOf(start) - 23 * HOUR;
    for (let h = 0; h < 24; h++) {
      const hour = firstHour + h * HOUR;
      const intensity = diurnal(new Date(hour).getHours());
      const bucket: HourBucket = { hour, builds: 0, spentSeconds: 0, savedSeconds: 0, forks: 0, parks: 0, queueWaitP95Seconds: 0 };
      const end = Math.min(hour + HOUR, start);
      if (end > hour) {
        const count = Math.round(intensity * 820 * ((end - hour) / HOUR) * this.rng.range(0.85, 1.15));
        for (let i = 0; i < count; i++) {
          const rec = this.synthesizeCommand(this.rng.range(hour, end));
          if (!rec) continue;
          this.history.push(rec);
          bucket.builds += 1;
          bucket.spentSeconds += rec.seconds;
          if (rec.coldSeconds !== null) bucket.savedSeconds += Math.max(0, rec.coldSeconds - rec.seconds);
          bucket.parks += rec.parks;
        }
        bucket.forks = Math.round(intensity * 14 * this.rng.range(0.7, 1.3));
      }
      bucket.queueWaitP95Seconds = Math.round(intensity * this.rng.range(4, 40));
      this.hourly.push(bucket);
      for (const spec of NODES) {
        const hours = (this.nodeHourly[spec.id] ??= []);
        const offline = !spec.online && hour > start + WARMUP_SECONDS * 1000 - 3.2 * HOUR;
        hours.push({
          hour,
          cpuBusy: offline ? 0 : clamp(intensity * this.rng.range(0.35, 0.8) * (spec.cpus < 10 ? 1.2 : 1), 0.02, 0.97),
          memoryUsed: offline ? 0 : clamp(0.18 + intensity * this.rng.range(0.25, 0.55), 0.1, 0.95),
        });
      }
    }
    this.history.sort((a, b) => a.endedAt - b.endedAt);
  }

  private synthesizeCommand(at: number): FinishedCommand | null {
    const repo = this.rng.pick(REPOS);
    const nodeId = this.rng.pick(HOSTS[repo.id] ?? []);
    const spec = this.spec(nodeId);
    const template = this.rng.pick(COMMANDS[repo.language].filter((t) => t.kind !== "server"));
    const rebuilt = this.rng.int(template.rebuilt[0], template.rebuilt[1]);
    const run = this.rng.range(template.run[0], template.run[1]);
    const perUnit = repo.coldBuildSeconds / repo.units;
    const seconds = (1.5 + rebuilt * perUnit * this.rng.range(1.2, 2.2) + this.rng.range(3, 8)) / spec.speed + run;
    const coldSeconds = repo.coldBuildSeconds / spec.speed + run;
    const failed = this.rng.chance(template.failRate);
    const failure: FailureKind | null = failed
      ? template.kind === "test"
        ? this.rng.chance(0.8) ? "test" : "compile"
        : template.argv.includes("clippy") ? "lint" : this.rng.chance(0.1) ? "signal" : "compile"
      : null;
    const parks = spec.memoryBytes <= 18 * GiB && this.rng.chance(0.04) ? 1 : 0;
    const { session, workspaceId } = this.pastSession(at, repo.id, nodeId);
    return {
      id: `c-${this.rng.hex(6)}`,
      sessionId: session.id,
      repoId: repo.id,
      nodeId,
      workspaceId,
      agent: `${this.rng.pick(AGENTS)}/${this.rng.pick(TOPICS[repo.id] ?? ["work"])}`,
      argv: template.argv.replace("{crate}", this.rng.pick(CRATES[repo.id] ?? ["core"])),
      kind: template.kind,
      state: failed ? "failed" : this.rng.chance(0.01) ? "cancelled" : "succeeded",
      exitCode: failed ? (failure === "signal" ? null : failure === "test" ? 100 : 101) : 0,
      failure,
      endedAt: at,
      seconds: seconds + parks * this.rng.range(20, 160),
      coldSeconds,
      rebuilt,
      of: repo.units,
      peakMemoryBytes: this.rng.range(template.memoryGiB[0], template.memoryGiB[1]) * GiB,
      parks,
    };
  }

  /**
   * The ended session a synthesized command at `at` belonged to: one of a few
   * per hour and a half, each with a workspace per repo it touched.
   */
  private pastSession(at: number, repoId: string, nodeId: string): { session: Session; workspaceId: string } {
    const window = 90 * 60_000;
    const from = Math.floor(at / window) * window;
    const key = `${from}:${this.rng.int(0, 5)}`;
    let session = this.past.get(key);
    if (!session) {
      const ended = Math.min(from + window + this.rng.range(5, 40) * 60_000, Math.floor(this.now / 1000) * 1000);
      session = {
        id: this.sessionName(),
        agent: this.rng.pick(AGENT_HOSTS),
        startedAt: iso(from),
        lastActiveAt: iso(from),
        endedAt: iso(ended),
        endReason: this.rng.chance(0.12) ? "abandoned: nothing touched it for an hour" : "ended by its agent",
        workspaces: [],
      };
      this.past.set(key, session);
    }
    let ws = session.workspaces.find((w) => w.repoId === repoId);
    if (!ws) {
      ws = { id: `ws-${this.rng.hex(4)}`, repoId, nodeId, createdAt: session.startedAt, releasedAt: session.endedAt };
      session.workspaces.push(ws);
    }
    if (at > Date.parse(session.lastActiveAt)) session.lastActiveAt = iso(at);
    return { session, workspaceId: ws.id };
  }

  // ---- helpers --------------------------------------------------------

  private node(spec: NodeSpec): Node {
    const usage = this.usage(spec.id);
    const bases = this.bases.filter((b) => b.nodeId === spec.id).reduce((n, b) => n + b.sizeBytes, 0);
    const forks = this.workspaces.filter((w) => w.nodeId === spec.id && w.state !== "released").reduce((n, w) => n + w.exclusiveBytes, 0);
    const other = spec.diskBytes * 0.06;
    return {
      id: spec.id,
      hostname: spec.hostname,
      arch: spec.arch,
      machine: spec.machine,
      kernel: spec.kernel,
      online: spec.online,
      joinedAt: iso(this.now - 19 * 24 * HOUR - spec.cpus * HOUR),
      lastSeen: iso(spec.online ? this.now - this.rng.range(0, 4000) : this.now - 3.2 * HOUR),
      cpus: spec.cpus,
      memoryBytes: spec.memoryBytes,
      swapBytes: spec.swapBytes,
      diskBytes: spec.diskBytes,
      usage: { ...usage },
      reservedMemoryBytes: this.reserved(spec.id),
      disk: { basesBytes: bases, forksExclusiveBytes: forks, otherBytes: other, freeBytes: Math.max(0, spec.diskBytes - bases - forks - other) },
    };
  }

  private totals() {
    const day = this.now - 24 * HOUR;
    const recent = this.history.filter((h) => h.endedAt > day);
    let saved = 0;
    for (const h of recent) if (h.coldSeconds !== null) saved += Math.max(0, h.coldSeconds - h.seconds);
    const sorted = [...this.forkMillis].sort((a, b) => a - b);
    return {
      savedSeconds: saved,
      builds: recent.length,
      forks: this.hourly.reduce((n, h) => n + h.forks, 0),
      medianForkMillis: sorted[Math.floor(sorted.length / 2)] ?? 0,
      oomKills: 0,
      parks: this.hourly.reduce((n, h) => n + h.parks, 0),
    };
  }

  private bumpHour(f: (h: HourBucket) => void): void {
    const hour = hourOf(this.now);
    let last = this.hourly[this.hourly.length - 1];
    if (!last || last.hour !== hour) {
      last = { hour, builds: 0, spentSeconds: 0, savedSeconds: 0, forks: 0, parks: 0, queueWaitP95Seconds: Math.round(this.rng.range(3, 25)) };
      this.hourly.push(last);
      if (this.hourly.length > 24) this.hourly.shift();
      for (const spec of NODES) {
        const hours = (this.nodeHourly[spec.id] ??= []);
        hours.push({ hour, cpuBusy: spec.online ? 0.3 : 0, memoryUsed: spec.online ? 0.4 : 0 });
        if (hours.length > 24) hours.shift();
      }
    }
    f(last);
  }

  private event(kind: EventKind, nodeId: string | null, text: string, ref: ClusterEvent["ref"]): void {
    this.events.unshift({ id: ++this.eventId, at: iso(this.now), kind, nodeId, text, ref });
    if (this.events.length > MAX_EVENTS) this.events.pop();
  }

  private memoryUsed(spec: NodeSpec): number {
    let used = spec.idleMemoryBytes;
    for (const c of this.commands) if (c.nodeId === spec.id && (c.state === "running" || c.state === "parked")) used += c.memoryBytes;
    return used;
  }

  private freeMemory(nodeId: string): number {
    const spec = this.spec(nodeId);
    return spec.memoryBytes - this.memoryUsed(spec);
  }

  private reserved(nodeId: string): number {
    let n = 0;
    for (const c of this.commands) if (c.nodeId === nodeId && (c.state === "running" || c.state === "parked")) n += c.limits.memoryLowBytes;
    return n;
  }

  private usage(nodeId: string): Node["usage"] {
    const u = this.nodeUsage.get(nodeId);
    if (!u) throw new Error(`no node ${nodeId}`);
    return u;
  }

  private spec(nodeId: string): NodeSpec {
    const s = this.specs.get(nodeId);
    if (!s) throw new Error(`no node ${nodeId}`);
    return s;
  }

  private run(commandId: string): Run {
    const r = this.runs.get(commandId);
    if (!r) throw new Error(`no run for ${commandId}`);
    return r;
  }
}

function repoOf(id: string): Repo {
  const r = REPOS.find((x) => x.id === id);
  if (!r) throw new Error(`no repo ${id}`);
  return r;
}

function isLive(c: { state: Command["state"] }): boolean {
  return c.state === "queued" || c.state === "running" || c.state === "parked";
}

function failureText(f: FailureKind | null): string {
  switch (f) {
    case "test":
      return "tests failed";
    case "compile":
      return "compile error";
    case "lint":
      return "clippy warnings denied";
    case "signal":
      return "killed by signal";
    default:
      return "non-zero exit";
  }
}

/** How busy the cluster is at an hour of the day, 0..1. Agents work nights too. */
function diurnal(hour: number): number {
  const day = Math.exp(-((hour - 14) ** 2) / 22);
  return 0.28 + 0.72 * day;
}

const iso = (t: number) => new Date(t).toISOString();
const hourOf = (t: number) => Math.floor(t / HOUR) * HOUR;
const clamp = (x: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, x));
const roundGiB = (b: number) => Math.max(0.25, Math.round((b / GiB) * 4) / 4) * GiB;
const gib = (b: number) => `${(b / GiB).toFixed(1)} GiB`;

function clock(seconds: number): string {
  if (seconds < 60) return `${seconds.toFixed(1)} s`;
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  return `${m}m${String(s).padStart(2, "0")}s`;
}


