import { useCallback, useMemo } from "react";

import { useResourceManager } from "./useResourceManager";

export interface UseAbortControllerResult {
  set: (controller: AbortController | null) => void;
  abort: () => void;
  abortAndCreate: () => AbortController;
  clearIfCurrent: (controller: AbortController) => void;
}

export interface AbortControllerManager extends UseAbortControllerResult {}

/**
 * Testable AbortController manager used by the hook.
 */
export function createAbortControllerManager(): AbortControllerManager {
  let current: AbortController | null = null;

  const set = (controller: AbortController | null) => {
    current = controller;
  };

  const abort = () => {
    current?.abort();
    current = null;
  };

  const abortAndCreate = () => {
    abort();
    const controller = new AbortController();
    current = controller;
    return controller;
  };

  const clearIfCurrent = (controller: AbortController) => {
    if (current === controller) {
      current = null;
    }
  };

  return { set, abort, abortAndCreate, clearIfCurrent };
}

/**
 * Tracks a single AbortController and aborts it automatically on unmount.
 */
export function useAbortController(): UseAbortControllerResult {
  const manager = useResourceManager(createAbortControllerManager, (currentManager) => currentManager.abort());

  const set = useCallback((controller: AbortController | null) => manager.set(controller), [manager]);
  const abort = useCallback(() => manager.abort(), [manager]);
  const abortAndCreate = useCallback(() => manager.abortAndCreate(), [manager]);
  const clearIfCurrent = useCallback((controller: AbortController) => manager.clearIfCurrent(controller), [manager]);

  return useMemo(
    () => ({ set, abort, abortAndCreate, clearIfCurrent }),
    [abort, abortAndCreate, clearIfCurrent, set],
  );
}
