import { useCallback, useMemo } from "react";

import { useResourceManager } from "./useResourceManager";

type TimerId = ReturnType<typeof setTimeout>;
type TimerSet = typeof setTimeout;
type TimerClear = typeof clearTimeout;

export interface TimeoutManager {
  schedule: (callback: () => void, delayMs: number) => void;
  clear: () => void;
}

/**
 * Testable timeout controller used by the hook.
 */
export function createTimeoutManager(
  setTimer: TimerSet = setTimeout,
  clearTimer: TimerClear = clearTimeout,
): TimeoutManager {
  let timerId: TimerId | null = null;

  const clear = () => {
    if (!timerId) return;
    clearTimer(timerId);
    timerId = null;
  };

  const schedule = (callback: () => void, delayMs: number) => {
    clear();
    timerId = setTimer(() => {
      timerId = null;
      callback();
    }, delayMs);
  };

  return { schedule, clear };
}

export interface UseTimeoutResult {
  schedule: (callback: () => void, delayMs: number) => void;
  clear: () => void;
}

/**
 * Manages a single timeout instance and always clears it on unmount.
 */
export function useTimeout(): UseTimeoutResult {
  const manager = useResourceManager(createTimeoutManager, (currentManager) => currentManager.clear());

  const clear = useCallback(() => manager.clear(), [manager]);
  const schedule = useCallback((callback: () => void, delayMs: number) => manager.schedule(callback, delayMs), [manager]);

  return useMemo(
    () => ({ schedule, clear }),
    [clear, schedule],
  );
}
