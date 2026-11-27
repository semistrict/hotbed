import { create } from "@bufbuild/protobuf";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { CgroupStatsSchema, MemoryRegionSchema, OpenFileSchema, SampledCgroupSchema, SampledOpenFileSchema } from "../api";
import { CgroupView, FilesTable, MemoryMapsList } from "./DiagnosticsViews";

describe("FilesTable", () => {
  it("offers the moved-offset and queued/locked pills only for sampled files that have them", () => {
    const live = renderToStaticMarkup(
      <FilesTable
        files={[
          create(OpenFileSchema, { pid: 1, comm: "app", fd: 3, kind: "file", target: "/tmp/a", pos: 4096n }),
          create(OpenFileSchema, { pid: 1, comm: "app", fd: 4, kind: "socket", socketProto: "tcp", localAddr: "127.0.0.1:80", socketState: "LISTEN" }),
        ]}
      />,
    );
    expect(live).toContain("all (2)");
    expect(live).toContain("file (1)");
    expect(live).toContain("socket (1)");
    expect(live).not.toContain("Δpos active");
    expect(live).not.toContain("queued / locked");
    expect(live).toContain("127.0.0.1:80");
    expect(live).toContain("4 KiB");

    const sampled = renderToStaticMarkup(
      <FilesTable
        files={[
          create(SampledOpenFileSchema, { pid: 2, comm: "cc", fd: 5, kind: "file", target: "/out.o", pos: 2048n, posDelta: 1024n }),
          create(SampledOpenFileSchema, { pid: 2, comm: "cc", fd: 6, kind: "socket", socketState: "ESTABLISHED", txQueueBytes: 512n, peer: "cladefsd" }),
        ]}
      />,
    );
    expect(sampled).toContain("Δpos active (1)");
    expect(sampled).toContain("queued / locked (1)");
    expect(sampled).toContain("(+1 KiB)");
    expect(sampled).toContain("↔ cladefsd");
    expect(sampled).toContain("tx 512 B · rx 0 B");
  });

  it("says why it is empty", () => {
    expect(renderToStaticMarkup(<FilesTable files={[]} empty="No open file descriptors found for PID 7." />)).toContain(
      "No open file descriptors found for PID 7.",
    );
  });
});

describe("CgroupView", () => {
  const stats = create(CgroupStatsSchema, {
    cpuUsageUsec: 3_500_000n,
    nrThrottled: 4n,
    memoryCurrentBytes: 1024n ** 3n,
    pidsCurrent: 3n,
    pidsPeak: 9n,
    psiCpuSomeAvg10: 1.5,
    io: [{ device: "259:0", rbytes: 1024n, rios: 2n }],
  });

  it("shows totals since the cgroup started", () => {
    const html = renderToStaticMarkup(<CgroupView stats={stats} />);
    expect(html).toContain("CPU total");
    expect(html).toContain("3.50s");
    expect(html).toContain("PIDs / Faults");
    expect(html).toContain("3 / 9");
    expect(html).toContain("PSI CPU avg10");
    expect(html).toContain("1.50%");
    expect(html).toContain("dev 259:0");
    expect(html).not.toContain("(Δ)");
  });

  it("shows what changed over a sample's window", () => {
    const window = create(SampledCgroupSchema, { end: stats, cpuCoresAvg: 1.25, nrThrottledDelta: 2n, psiIoSomeDeltaUsec: 12_000n });
    const html = renderToStaticMarkup(<CgroupView stats={stats} window={window} />);
    expect(html).toContain("Cgroup CPU avg");
    expect(html).toContain("1.25 cores");
    expect(html).toContain("CPU Throttled (Δ)");
    expect(html).toContain("(total 4)");
    expect(html).toContain("PSI I/O stall (Δ)");
    expect(html).toContain("+12.0 ms");
    expect(html).not.toContain("CPU total");
  });
});

describe("MemoryMapsList", () => {
  it("caps the regions shown and names the security context when known", () => {
    const topRegions = Array.from({ length: 10 }, (_, i) => create(MemoryRegionSchema, { perms: "r-xp", path: `/lib/region-${i}.so`, sizeBytes: 4096n }));
    const html = renderToStaticMarkup(
      <MemoryMapsList
        maps={[{ pid: 9, comm: "rustc", rssBytes: 0n, pssBytes: 0n, privateDirtyBytes: 0n, swapBytes: 0n, seccomp: "filter", topRegions }]}
        maxRegions={8}
      />,
    );
    expect(html).toContain("PID 9 (rustc)");
    expect(html).toContain("seccomp: filter");
    expect(html).toContain("/lib/region-7.so");
    expect(html).not.toContain("/lib/region-8.so");
  });
});
