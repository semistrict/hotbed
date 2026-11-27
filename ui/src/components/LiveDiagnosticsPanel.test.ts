import { create } from "@bufbuild/protobuf";
import { createElement, isValidElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { InspectCommandResponseSchema, InspectSection } from "../api";
import {
  InspectReport,
  inspectReportReducer,
  InspectReportView,
  mergeInspectResult,
  type InspectReportState,
} from "./LiveDiagnosticsPanel";

function collectButtons(node: ReactNode): ReactElement<{ onClick?: () => void; children?: ReactNode }>[] {
  const out: ReactElement<{ onClick?: () => void; children?: ReactNode }>[] = [];
  const visit = (n: ReactNode) => {
    if (!n || typeof n !== "object") return;
    if (Array.isArray(n)) {
      for (const child of n) visit(child);
      return;
    }
    if (!isValidElement<{ children?: ReactNode; onClick?: () => void }>(n)) return;
    if (n.type === "button") {
      out.push(n);
    }
    visit(n.props.children);
  };
  visit(node);
  return out;
}

describe("LiveDiagnosticsPanel mergeInspectResult", () => {
  it("preserves previously fetched sections for the same PID and drops stale per-PID sections when PID changes", () => {
    const filesRes = create(InspectCommandResponseSchema, {
      files: [{ pid: 100, comm: "app", fd: 3, kind: "file", target: "/tmp/a" }],
      cgroupStats: { pidsCurrent: 2n },
    });
    const state1 = mergeInspectResult(null, filesRes, 100, InspectSection.FILES);
    expect(state1.pid).toBe(100);
    expect(state1.data.files).toHaveLength(1);

    // Same PID, different tab: keeps PID 100's files alongside PID 100's maps.
    const mapsRes100 = create(InspectCommandResponseSchema, {
      maps: [{ pid: 100, comm: "app", rssBytes: 4096n }],
    });
    const state2 = mergeInspectResult(state1, mapsRes100, 100, InspectSection.MAPS);
    expect(state2.data.files).toHaveLength(1);
    expect(state2.data.maps).toHaveLength(1);

    // Different PID (200): must NOT retain PID 100's files or maps.
    const mapsRes200 = create(InspectCommandResponseSchema, {
      maps: [{ pid: 200, comm: "worker", rssBytes: 8192n }],
    });
    const state3 = mergeInspectResult(state2, mapsRes200, 200, InspectSection.MAPS);
    expect(state3.pid).toBe(200);
    expect(state3.data.files).toEqual([]);
    expect(state3.data.kstacks).toEqual([]);
    expect(state3.data.limits).toEqual([]);
    expect(state3.data.maps[0]?.pid).toBe(200);
    // Command-wide cgroup stats remain intact.
    expect(state3.data.cgroupStats?.pidsCurrent).toBe(2n);
  });
});

describe("InspectReport PID filtering", () => {
  const inspectResult = create(InspectCommandResponseSchema, {
    commandId: "cmd-1",
    cgroup: "/hotbed/cmd-1",
    procs: [
      { pid: 412, ppid: 1, comm: "cargo-nextest", cmdline: "cargo nextest run", state: "S", threadsCount: 2 },
      { pid: 515, ppid: 412, comm: "rustc", cmdline: "rustc --edition 2021", state: "R", threadsCount: 4 },
    ],
    files: [
      { pid: 412, comm: "cargo-nextest", fd: 3, kind: "file", target: "/tmp/nextest.log" },
      { pid: 412, comm: "cargo-nextest", fd: 4, kind: "socket", socketProto: "tcp", localAddr: "127.0.0.1:8080", socketState: "LISTEN" },
      { pid: 515, comm: "rustc", fd: 7, kind: "file", target: "/tmp/rustc.rmeta" },
    ],
    maps: [
      { pid: 412, comm: "cargo-nextest", rssBytes: 4096n },
      { pid: 515, comm: "rustc", rssBytes: 8192n },
    ],
  });

  it("filters FILES strictly to the clicked PID when clicking FDs and allows clearing the filter", () => {
    let state: InspectReportState = { tab: InspectSection.PROCS, selectedPid: null };
    const dispatch = (action: Parameters<typeof inspectReportReducer>[1]) => {
      state = inspectReportReducer(state, action);
    };

    // Start on PROCS: InspectReportView wires ProcsView's onPickPid to dispatch pick_pid.
    const procsTree = InspectReportView({
      result: inspectResult,
      tab: state.tab,
      selectedPid: state.selectedPid,
      onSelectTab: (tab) => dispatch({ type: "set_tab", tab }),
      onPickPid: (pid) => dispatch({ type: "pick_pid", pid }),
      onClearPid: () => dispatch({ type: "clear_pid" }),
    });
    const procsViewEl = (procsTree.props as { children: ReactNode[] }).children.find(
      (c): c is ReactElement<{ children?: ReactNode }> => isValidElement(c) && c.type === "div" && Boolean((c.props as { className?: string }).className?.includes("rounded-xl")),
    );
    expect(procsViewEl).toBeDefined();
    // Simulate clicking the FDs button for PID 412 (via ProcsView's onPickPid prop).
    const procsChild = (procsViewEl!.props.children as ReactNode[]).find((c): c is ReactElement<{ onPickPid: (pid: number) => void }> =>
      isValidElement(c),
    );
    expect(procsChild).toBeDefined();
    procsChild!.props.onPickPid(412);

    expect(state).toEqual({ tab: InspectSection.FILES, selectedPid: 412 });

    // Rendered FILES view with selectedPid=412 must include only PID 412's files and show the active filter pill.
    const filteredHtml = renderToStaticMarkup(
      createElement(InspectReport, { result: inspectResult, initialTab: state.tab, initialPid: state.selectedPid }),
    );
    expect(filteredHtml).toContain("Filtered to PID 412 (cargo-nextest)");
    expect(filteredHtml).toContain("Show all PIDs");
    expect(filteredHtml).toContain("/tmp/nextest.log");
    expect(filteredHtml).toContain("127.0.0.1:8080");
    expect(filteredHtml).not.toContain("/tmp/rustc.rmeta");

    // Switching tabs retains the PID filter on other per-PID views (e.g. MAPS).
    dispatch({ type: "set_tab", tab: InspectSection.MAPS });
    const filteredMapsHtml = renderToStaticMarkup(
      createElement(InspectReport, { result: inspectResult, initialTab: state.tab, initialPid: state.selectedPid }),
    );
    expect(filteredMapsHtml).toContain("PID 412 (cargo-nextest)");
    expect(filteredMapsHtml).not.toContain("PID 515 (rustc)");

    // Clicking the "Show all PIDs" button clears the PID filter.
    dispatch({ type: "set_tab", tab: InspectSection.FILES });
    const filesTree = InspectReportView({
      result: inspectResult,
      tab: state.tab,
      selectedPid: state.selectedPid,
      onSelectTab: (tab) => dispatch({ type: "set_tab", tab }),
      onPickPid: (pid) => dispatch({ type: "pick_pid", pid }),
      onClearPid: () => dispatch({ type: "clear_pid" }),
    });
    const clearBtn = collectButtons(filesTree).find((b) => b.props.children === "Show all PIDs");
    expect(clearBtn).toBeDefined();
    clearBtn!.props.onClick?.();

    expect(state).toEqual({ tab: InspectSection.FILES, selectedPid: null });

    const unfilteredHtml = renderToStaticMarkup(
      createElement(InspectReport, { result: inspectResult, initialTab: state.tab, initialPid: state.selectedPid }),
    );
    expect(unfilteredHtml).not.toContain("Filtered to PID");
    expect(unfilteredHtml).toContain("/tmp/nextest.log");
    expect(unfilteredHtml).toContain("/tmp/rustc.rmeta");
  });
});
