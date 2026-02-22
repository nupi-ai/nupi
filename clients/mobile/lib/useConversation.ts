import { useCallback, useEffect, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";

import type { NupiClient } from "./connect";
import type { ConversationTurn as ProtoTurn } from "./gen/nupi_pb";

export interface ConversationTurn {
  origin: "user" | "ai" | "system" | "tool";
  text: string;
  at: Date;
  metadata: Record<string, string>;
  isOptimistic?: boolean;
}

const KNOWN_ORIGINS = new Set<ConversationTurn["origin"]>(["user", "ai", "system", "tool"]);

function mapProtoTurn(turn: ProtoTurn): ConversationTurn {
  const metadata: Record<string, string> = {};
  for (const m of turn.metadata) {
    metadata[m.key] = m.value;
  }
  const origin = KNOWN_ORIGINS.has(turn.origin as ConversationTurn["origin"])
    ? (turn.origin as ConversationTurn["origin"])
    : "system";
  return {
    origin,
    text: turn.text,
    at: turn.at ? timestampDate(turn.at) : new Date(),
    metadata,
  };
}

/** Polling phase intervals (ms). */
const FAST_INTERVAL = 500;
const SLOW_INTERVAL = 2000;
/** Phase boundary durations from polling start (ms). */
const FAST_PHASE_MS = 15_000;
const SLOW_PHASE_MS = 60_000;
/** Limits for initial fetch and delta polls. */
const INITIAL_LIMIT = 20;
const POLL_LIMIT = 10;

/**
 * Custom hook for conversation history management with adaptive polling.
 *
 * Fetches conversation turns from the server, supports delta polling
 * after voice commands, and provides optimistic UI updates.
 *
 * Polling phases after startPolling():
 *   0-15s  → poll every 500ms (fast — AI typically responds in 2-5s)
 *   15-60s → poll every 2s   (slow — complex tool-use or slow AI)
 *   60s+   → auto-stop       (assume timeout)
 */
export function useConversation(
  sessionId: string,
  client: NupiClient | null,
  /** When false, polling is paused (e.g. during network disconnection). */
  isConnected = true,
) {
  const [turns, setTurns] = useState<ConversationTurn[]>([]);
  const [isLoading, setIsLoading] = useState(false);
  const [isPolling, setIsPolling] = useState(false);
  // H3 fix: expose poll/fetch errors to the UI.
  const [error, setError] = useState<string | null>(null);

  const mountedRef = useRef(true);
  const pollingRef = useRef(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pollStartRef = useRef(0);
  const fetchingRef = useRef(false);
  // M3 fix: AbortController for cancelling in-flight requests on cleanup.
  const abortRef = useRef<AbortController | null>(null);
  // L3 fix: AbortController for cancelling in-flight refresh requests.
  const refreshAbortRef = useRef<AbortController | null>(null);
  // Tracks server's total turn count — used as offset for delta fetching.
  // Set from GetConversation response.total on every successful fetch.
  const serverTotalRef = useRef(0);
  // M2 fix: track isConnected via ref for access inside polling callbacks.
  const isConnectedRef = useRef(isConnected);
  isConnectedRef.current = isConnected;
  // M1 fix (Review 9): consecutive error counter for poll backoff.
  const consecutiveErrorsRef = useRef(0);

  const fetchTurns = useCallback(
    async (offset: number, limit: number, signal?: AbortSignal) => {
      if (!client) return null;
      try {
        if (sessionId) {
          const resp = await client.sessions.getConversation({ sessionId, offset, limit }, { signal });
          setError(null);
          return { turns: resp.turns.map(mapProtoTurn), total: resp.total };
        }
        const resp = await client.sessions.getGlobalConversation({ offset, limit }, { signal });
        setError(null);
        return { turns: resp.turns.map(mapProtoTurn), total: resp.total };
      } catch (e) {
        // Don't report abort as an error.
        if (signal?.aborted) return null;
        setError(e instanceof Error ? e.message : "Failed to fetch conversation");
        return null;
      }
    },
    [client, sessionId],
  );

  // Ref for the latest poll tick — timers call through this to avoid stale closures.
  const pollTickRef = useRef<() => void>(() => {});

  useEffect(() => {
    pollTickRef.current = async () => {
      // M2 fix: skip poll tick when disconnected.
      if (!pollingRef.current || !mountedRef.current || fetchingRef.current || !client || !isConnectedRef.current) return;
      fetchingRef.current = true;

      // M3 fix: use AbortController so cleanup can cancel in-flight polls.
      const controller = new AbortController();
      abortRef.current = controller;
      const offset = serverTotalRef.current;
      const result = await fetchTurns(offset, POLL_LIMIT, controller.signal);
      abortRef.current = null;
      fetchingRef.current = false;

      if (!pollingRef.current || !mountedRef.current) return;

      if (!result) {
        // M1 fix (Review 9): backoff on consecutive poll errors to avoid
        // hammering an unresponsive server (~45 failed requests in 60s).
        consecutiveErrorsRef.current++;
      } else {
        consecutiveErrorsRef.current = 0;

        // L1 fix (Review 9): detect server-side conversation truncation.
        // If server total dropped below our tracked offset, reset to avoid
        // NOP polling where result.total > offset is never true.
        if (result.total < offset) {
          serverTotalRef.current = result.total;
        }

        if (result.total > offset) {
          serverTotalRef.current = result.total;
          const incoming = result.turns;
          setTurns(prev => {
            const real = prev.filter(t => !t.isOptimistic);
            const optimistic = [...prev.filter(t => t.isOptimistic)];
            // Remove optimistic entries that match an incoming real turn (one-for-one).
            // Use origin + normalized text comparison to handle server-side trimming.
            const normalize = (s: string) => s.trim().toLowerCase();
            for (const n of incoming) {
              const idx = optimistic.findIndex(
                opt => opt.origin === n.origin && normalize(opt.text) === normalize(n.text),
              );
              if (idx !== -1) optimistic.splice(idx, 1);
            }
            return [...real, ...incoming, ...optimistic];
          });
        }
      }

      // Schedule next poll
      if (!pollingRef.current || !mountedRef.current) return;
      const elapsed = Date.now() - pollStartRef.current;
      if (elapsed >= SLOW_PHASE_MS) {
        pollingRef.current = false;
        setIsPolling(false);
        // M3 fix: clear stale error when polling auto-stops.
        setError(null);
        return;
      }
      let interval = elapsed < FAST_PHASE_MS ? FAST_INTERVAL : SLOW_INTERVAL;
      // M1 fix (Review 9): after 3+ consecutive errors, backoff to slow interval.
      if (consecutiveErrorsRef.current >= 3) interval = SLOW_INTERVAL;
      timerRef.current = setTimeout(() => pollTickRef.current(), interval);
    };
  }, [client, fetchTurns]);

  // Initial fetch on mount / when client or sessionId changes.
  // Two-step: first get total count, then fetch the most recent turns.
  useEffect(() => {
    mountedRef.current = true;
    if (!client) return;

    const controller = new AbortController();
    setIsLoading(true);
    setError(null);

    (async () => {
      // Probe with limit=0 to discover total count without transferring turns.
      const probe = await fetchTurns(0, 0, controller.signal);
      if (controller.signal.aborted) return;
      if (!probe) {
        setIsLoading(false);
        return;
      }
      // Fetch the most recent INITIAL_LIMIT turns.
      const recentOffset = Math.max(0, probe.total - INITIAL_LIMIT);
      const result = await fetchTurns(recentOffset, INITIAL_LIMIT, controller.signal);
      if (controller.signal.aborted) return;
      if (result) {
        serverTotalRef.current = result.total;
        setTurns(result.turns);
      }
      setIsLoading(false);
    })();

    return () => {
      controller.abort();
    };
  }, [client, sessionId, fetchTurns]);

  const startPolling = useCallback(() => {
    // M2 fix (Review 6): clear stale error from previous polling session so
    // the error banner doesn't persist when a new voice command starts polling.
    setError(null);
    // M1 fix (Review 9): reset error counter on new polling session.
    consecutiveErrorsRef.current = 0;
    // H1 fix: restart fast phase when a new voice command arrives during slow phase.
    pollStartRef.current = Date.now();
    if (pollingRef.current) {
      // Cancel current (potentially slow-phase) timer and reschedule at fast interval.
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = setTimeout(() => pollTickRef.current(), FAST_INTERVAL);
      }
      return;
    }
    pollingRef.current = true;
    setIsPolling(true);
    timerRef.current = setTimeout(() => pollTickRef.current(), FAST_INTERVAL);
  }, []);

  const stopPolling = useCallback(() => {
    pollingRef.current = false;
    if (timerRef.current) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    setIsPolling(false);
  }, []);

  const refresh = useCallback(async () => {
    if (!client) return;
    // L3 fix: abort any previous in-flight refresh and use AbortController.
    refreshAbortRef.current?.abort();
    const controller = new AbortController();
    refreshAbortRef.current = controller;
    setIsLoading(true);
    // Probe total, then fetch most recent turns.
    const probe = await fetchTurns(0, 0, controller.signal);
    if (!probe || !mountedRef.current) {
      // Only clear ref if still ours — a concurrent refresh may have superseded.
      if (refreshAbortRef.current === controller) refreshAbortRef.current = null;
      setIsLoading(false);
      return;
    }
    const recentOffset = Math.max(0, probe.total - INITIAL_LIMIT);
    const result = await fetchTurns(recentOffset, INITIAL_LIMIT, controller.signal);
    // Only clear ref if still ours — a concurrent refresh may have superseded.
    if (refreshAbortRef.current === controller) refreshAbortRef.current = null;
    if (!mountedRef.current) return;
    if (result) {
      serverTotalRef.current = result.total;
      setTurns(result.turns);
    }
    setIsLoading(false);
  }, [client, fetchTurns]);

  const addOptimistic = useCallback((text: string) => {
    setTurns(prev => [
      ...prev,
      {
        origin: "user" as const,
        text,
        at: new Date(),
        metadata: {},
        isOptimistic: true,
      },
    ]);
  }, []);

  const removeLastOptimistic = useCallback(() => {
    setTurns(prev => {
      const idx = prev.findLastIndex(t => t.isOptimistic);
      if (idx === -1) return prev;
      return [...prev.slice(0, idx), ...prev.slice(idx + 1)];
    });
  }, []);

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      mountedRef.current = false;
      pollingRef.current = false;
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      // M3 fix: abort any in-flight request on unmount.
      abortRef.current?.abort();
      abortRef.current = null;
      // L3 fix: abort any in-flight refresh on unmount.
      refreshAbortRef.current?.abort();
      refreshAbortRef.current = null;
    };
  }, []);

  return { turns, isLoading, isPolling, error, startPolling, stopPolling, refresh, addOptimistic, removeLastOptimistic };
}
