import { describe, expect, it } from "vitest";

import { createAbortControllerManager } from "./useAbortController";

describe("createAbortControllerManager", () => {
  it("aborts currently tracked controller", () => {
    const manager = createAbortControllerManager();
    const controller = new AbortController();

    manager.set(controller);
    manager.abort();

    expect(controller.signal.aborted).toBe(true);
  });

  it("abortAndCreate aborts previous and tracks new controller", () => {
    const manager = createAbortControllerManager();
    const previous = new AbortController();
    manager.set(previous);

    const next = manager.abortAndCreate();

    expect(previous.signal.aborted).toBe(true);
    expect(next.signal.aborted).toBe(false);
  });

  it("clearIfCurrent clears only matching controller", () => {
    const manager = createAbortControllerManager();
    const current = new AbortController();
    const other = new AbortController();
    manager.set(current);

    manager.clearIfCurrent(other);
    manager.abort();
    expect(current.signal.aborted).toBe(true);

    const current2 = new AbortController();
    manager.set(current2);
    manager.clearIfCurrent(current2);
    manager.abort();
    expect(current2.signal.aborted).toBe(false);
  });
});
