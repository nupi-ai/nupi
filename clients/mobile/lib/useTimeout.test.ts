import { describe, expect, it } from "vitest";

import { createTimeoutManager } from "./useTimeout";

type TimerCallback = () => void;

function createFakeTimers() {
  let nextId = 1;
  const callbacks = new Map<number, TimerCallback>();
  let clearCalls = 0;

  const setTimer = ((callback: TimerCallback) => {
    const id = nextId++;
    callbacks.set(id, callback);
    return id as unknown as ReturnType<typeof setTimeout>;
  }) as typeof setTimeout;

  const clearTimer = ((id: ReturnType<typeof setTimeout>) => {
    clearCalls += 1;
    callbacks.delete(id as unknown as number);
  }) as typeof clearTimeout;

  const run = (id: number) => {
    const callback = callbacks.get(id);
    if (!callback) return;
    callbacks.delete(id);
    callback();
  };

  return {
    setTimer,
    clearTimer,
    run,
    get pendingCount() {
      return callbacks.size;
    },
    get clearCalls() {
      return clearCalls;
    },
  };
}

describe("createTimeoutManager", () => {
  it("schedules and executes callback", () => {
    const fake = createFakeTimers();
    const manager = createTimeoutManager(fake.setTimer, fake.clearTimer);
    let called = 0;

    manager.schedule(() => {
      called += 1;
    }, 100);
    expect(fake.pendingCount).toBe(1);

    fake.run(1);
    expect(called).toBe(1);
    expect(fake.pendingCount).toBe(0);
  });

  it("clears previous timeout before scheduling a new one", () => {
    const fake = createFakeTimers();
    const manager = createTimeoutManager(fake.setTimer, fake.clearTimer);
    let called = 0;

    manager.schedule(() => {
      called += 1;
    }, 100);
    manager.schedule(() => {
      called += 10;
    }, 200);

    expect(fake.clearCalls).toBe(1);
    expect(fake.pendingCount).toBe(1);

    fake.run(1);
    expect(called).toBe(0);

    fake.run(2);
    expect(called).toBe(10);
  });

  it("clear removes pending timeout", () => {
    const fake = createFakeTimers();
    const manager = createTimeoutManager(fake.setTimer, fake.clearTimer);
    let called = false;

    manager.schedule(() => {
      called = true;
    }, 100);
    manager.clear();

    expect(fake.pendingCount).toBe(0);
    expect(fake.clearCalls).toBe(1);
    fake.run(1);
    expect(called).toBe(false);
  });
});
