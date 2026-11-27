import { describe, expect, it } from "vitest";
import { BoardTaskState } from "../api";
import { isNavActive, routeParent, routeTitle } from "../components/Shell";
import { emojiDisplay, parseLabelInput, taskStateMeta } from "./Board";

describe("Board UI helpers & routing", () => {
  it("normalizes emoji display and label inputs", () => {
    expect(emojiDisplay("thumbs_up")).toBe("👍");
    expect(emojiDisplay("custom_emoji")).toBe(":custom_emoji:");
    expect(parseLabelInput("area=cli, status=open invalid")).toEqual({
      area: "cli",
      status: "open",
    });
  });

  it("maps task states to labels", () => {
    expect(taskStateMeta(BoardTaskState.READY).label).toBe("ready");
    expect(taskStateMeta(BoardTaskState.CLAIMED).label).toBe("claimed");
    expect(taskStateMeta(BoardTaskState.BLOCKED).label).toBe("blocked");
    expect(taskStateMeta(BoardTaskState.WAITING).label).toBe("waiting");
    expect(taskStateMeta(BoardTaskState.COMPLETED).label).toBe("completed");
    expect(taskStateMeta(BoardTaskState.FAILED).label).toBe("failed");
    expect(taskStateMeta(BoardTaskState.CANCELED).label).toBe("canceled");
  });

  it("registers /board and /tasks in Shell navigation", () => {
    expect(isNavActive("/board", "/board")).toBe(true);
    expect(isNavActive("/board/14378", "/board")).toBe(true);
    expect(routeTitle("/board")).toBe("Board");
    expect(routeTitle("/board/14378")).toBe("Board");
    expect(routeParent("/board/14378")).toBe("/board");
    expect(isNavActive("/tasks", "/tasks")).toBe(true);
    expect(routeTitle("/tasks")).toBe("Tasks");
  });
});
