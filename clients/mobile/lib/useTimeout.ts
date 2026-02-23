import { useCallback, useEffect, useMemo, useRef } from "react";

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
  const managerRef = useRef<TimeoutManager | null>(null);
  if (!managerRef.current) {
    managerRef.current = createTimeoutManager();
  }
  const manager = managerRef.current;

  const clear = useCallback(() => manager.clear(), [manager]);
  const schedule = useCallback((callback: () => void, delayMs: number) => manager.schedule(callback, delayMs), [manager]);

  useEffect(() => clear, [clear]);

  return useMemo(
    () => ({ schedule, clear }),
    [clear, schedule],
  );
}
