import { useCallback, useEffect, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";

import type { NupiClient } from "./connect";
import type { ConversationTurn as ProtoTurn } from "./gen/sessions_pb";
import { raceTimeout } from "./raceTimeout";
import { useAbortController } from "./useAbortController";
import { useTimeout } from "./useTimeout";

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
/** Per-request timeout for poll ticks (ms). Prevents a single stuck RPC
 * from blocking the entire polling window (fetchingRef stays true). */
const POLL_REQUEST_TIMEOUT = 5_000;
/** Limits for initial fetch and delta polls. */
const INITIAL_LIMIT = 20;
const POLL_LIMIT = 10;

interface PollState {
  active: boolean;
  inFlightToken: number | null;
  startedAt: number;
  consecutiveErrors: number;
  token: number;
}

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
  const pollStateRef = useRef<PollState>({
    active: false,
    inFlightToken: null,
    startedAt: 0,
    consecutiveErrors: 0,
    token: 0,
  });
  const pollTimer = useTimeout();
  // M3 fix: AbortController for cancelling in-flight requests on cleanup.
  const pollAbort = useAbortController();
  // L3 fix: AbortController for cancelling in-flight refresh requests.
  const refreshAbort = useAbortController();
  // Tracks server's total turn count — used as offset for delta fetching.
  // Set from GetConversation response.total on every successful fetch.
  const serverTotalRef = useRef(0);
  // M2 fix: track isConnected via ref for access inside polling callbacks.
  const isConnectedRef = useRef(isConnected);
  isConnectedRef.current = isConnected;

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

  const fetchRecentTurns = useCallback(
    async (signal?: AbortSignal) => {
      // Probe with limit=0 to discover total count without transferring turns.
      const probe = await fetchTurns(0, 0, signal);
      if (!probe || signal?.aborted) return null;
      // Fetch the most recent INITIAL_LIMIT turns.
      const recentOffset = Math.max(0, probe.total - INITIAL_LIMIT);
      return fetchTurns(recentOffset, INITIAL_LIMIT, signal);
    },
    [fetchTurns],
  );

  // Ref for the latest poll tick — timers call through this to avoid stale closures.
  const pollTickRef = useRef<(token: number) => void>(() => {});

  useEffect(() => {
    pollTickRef.current = async (token: number) => {
      const state = pollStateRef.current;
      // M2 fix: skip poll tick when disconnected.
      if (!state.active || state.token !== token || !mountedRef.current || state.inFlightToken === token || !client || !isConnectedRef.current) return;
      state.inFlightToken = token;

      // M3 fix: use AbortController so cleanup can cancel in-flight polls.
      // H1 fix (Review 11): per-request timeout prevents a stuck RPC from
      // blocking all subsequent polls.
      const controller = new AbortController();
      pollAbort.set(controller);
      const offset = serverTotalRef.current;
      let result: Awaited<ReturnType<typeof fetchTurns>> = null;
      try {
        result = await raceTimeout(
          fetchTurns(offset, POLL_LIMIT, controller.signal),
          POLL_REQUEST_TIMEOUT,
          "Conversation poll request timed out",
        );
      } catch {
        controller.abort();
        result = null;
      } finally {
        pollAbort.clearIfCurrent(controller);
        if (state.inFlightToken === token) {
          state.inFlightToken = null;
        }
      }

      if (!state.active || state.token !== token || !mountedRef.current) return;

      if (!result) {
        // M1 fix (Review 9): backoff on consecutive poll errors to avoid
        // hammering an unresponsive server (~45 failed requests in 60s).
        state.consecutiveErrors += 1;
      } else {
        state.consecutiveErrors = 0;

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
      if (!state.active || state.token !== token || !mountedRef.current) return;
      const elapsed = Date.now() - state.startedAt;
      if (elapsed >= SLOW_PHASE_MS) {
        state.active = false;
        setIsPolling(false);
        // M3 fix: clear stale error when polling auto-stops.
        setError(null);
        return;
      }
      let interval = elapsed < FAST_PHASE_MS ? FAST_INTERVAL : SLOW_INTERVAL;
      // M1 fix (Review 9): after 3+ consecutive errors, backoff to slow interval.
      if (state.consecutiveErrors >= 3) interval = SLOW_INTERVAL;
      pollTimer.schedule(() => pollTickRef.current(token), interval);
    };
  }, [client, fetchTurns, pollAbort, pollTimer]);

  // Initial fetch on mount / when client or sessionId changes.
  // Two-step: first get total count, then fetch the most recent turns.
  useEffect(() => {
    mountedRef.current = true;
    if (!client) return;

    const controller = new AbortController();
    setIsLoading(true);
    setError(null);

    (async () => {
      const result = await fetchRecentTurns(controller.signal);
      if (controller.signal.aborted || !mountedRef.current) return;
      if (!result) {
        setIsLoading(false);
        return;
      }
      serverTotalRef.current = result.total;
      setTurns(result.turns);
      setIsLoading(false);
    })();

    return () => {
      controller.abort();
    };
  }, [client, sessionId, fetchRecentTurns]);

  const startPolling = useCallback(() => {
    if (!mountedRef.current) return;
    // M2 fix (Review 6): clear stale error from previous polling session so
    // the error banner doesn't persist when a new voice command starts polling.
    setError(null);
    const state = pollStateRef.current;
    // M1 fix (Review 9): reset error counter on new polling session.
    state.consecutiveErrors = 0;
    // H1 fix: restart fast phase when a new voice command arrives during slow phase.
    state.startedAt = Date.now();
    if (state.active) {
      // Cancel current (potentially slow-phase) timer and reschedule at fast interval.
      pollTimer.schedule(() => pollTickRef.current(state.token), FAST_INTERVAL);
      return;
    }
    state.active = true;
    state.token += 1;
    setIsPolling(true);
    pollTimer.schedule(() => pollTickRef.current(state.token), FAST_INTERVAL);
  }, [pollTimer]);

  const stopPolling = useCallback(() => {
    const state = pollStateRef.current;
    state.active = false;
    state.token += 1;
    pollTimer.clear();
    pollAbort.abort();
    setIsPolling(false);
  }, [pollAbort, pollTimer]);

  const refresh = useCallback(async () => {
    if (!client) return;
    // L3 fix: abort any previous in-flight refresh and use AbortController.
    const controller = refreshAbort.abortAndCreate();
    setIsLoading(true);
    const result = await fetchRecentTurns(controller.signal);
    if (!result || !mountedRef.current) {
      // Only clear ref if still ours — a concurrent refresh may have superseded.
      refreshAbort.clearIfCurrent(controller);
      setIsLoading(false);
      return;
    }
    // Only clear ref if still ours — a concurrent refresh may have superseded.
    refreshAbort.clearIfCurrent(controller);
    if (!mountedRef.current) return;
    serverTotalRef.current = result.total;
    setTurns(result.turns);
    setIsLoading(false);
  }, [client, fetchRecentTurns, refreshAbort]);

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
      const state = pollStateRef.current;
      state.active = false;
      state.token += 1;
      // M3 fix: abort any in-flight request on unmount.
      pollAbort.abort();
      // L3 fix: abort any in-flight refresh on unmount.
      refreshAbort.abort();
    };
  }, [pollAbort, refreshAbort]);

  return { turns, isLoading, isPolling, error, startPolling, stopPolling, refresh, addOptimistic, removeLastOptimistic };
}
