import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import type { NupiClient } from "./connect";
import type { ConversationTurn } from "./conversationTypes";
import type { ConversationTurn as ProtoTurn } from "./gen/sessions_pb";

const KNOWN_ORIGINS = new Set<ConversationTurn["origin"]>(["user", "ai", "system", "tool"]);
const FAST_INTERVAL = 500;
const SLOW_INTERVAL = 2000;
const FAST_PHASE_MS = 15_000;
const SLOW_PHASE_MS = 60_000;
const INITIAL_LIMIT = 20;

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

async function fetchRecentConversation(sessionId: string, client: NupiClient, signal?: AbortSignal): Promise<ConversationTurn[]> {
  const probe = sessionId
    ? await client.sessions.getConversation({ sessionId, offset: 0, limit: 0 }, { signal })
    : await client.sessions.getGlobalConversation({ offset: 0, limit: 0 }, { signal });
  const recentOffset = Math.max(0, probe.total - INITIAL_LIMIT);
  const recent = sessionId
    ? await client.sessions.getConversation({ sessionId, offset: recentOffset, limit: INITIAL_LIMIT }, { signal })
    : await client.sessions.getGlobalConversation({ offset: recentOffset, limit: INITIAL_LIMIT }, { signal });
  return recent.turns.map(mapProtoTurn);
}

export function conversationQueryKey(sessionId: string) {
  return ["conversation", sessionId || "global"] as const;
}

export function conversationOptimisticQueryKey(sessionId: string) {
  return ["conversation-optimistic", sessionId || "global"] as const;
}

interface PollingIntervalInput {
  isPolling: boolean;
  pollStartedAt: number | null;
  isConnected: boolean;
  consecutiveErrors: number;
  now?: number;
}

export function getPollingInterval(input: PollingIntervalInput): number | false {
  const { isPolling, pollStartedAt, isConnected, consecutiveErrors } = input;
  if (!isPolling || !pollStartedAt || !isConnected) return false;
  const now = input.now ?? Date.now();
  const elapsed = now - pollStartedAt;
  if (elapsed >= SLOW_PHASE_MS) return false;
  if (consecutiveErrors >= 3) return SLOW_INTERVAL;
  return elapsed < FAST_PHASE_MS ? FAST_INTERVAL : SLOW_INTERVAL;
}

export function useConversationQuery(
  sessionId: string,
  client: NupiClient | null,
  isConnected = true,
) {
  const queryClient = useQueryClient();
  const [isPolling, setIsPolling] = useState(false);
  const [pollStartedAt, setPollStartedAt] = useState<number | null>(null);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const consecutiveErrorsRef = useRef(0);
  const prevConnectedRef = useRef(isConnected);
  const optimisticQueryKey = conversationOptimisticQueryKey(sessionId);

  const query = useQuery({
    queryKey: conversationQueryKey(sessionId),
    enabled: Boolean(client),
    queryFn: async ({ signal }) => {
      if (!client) return [] as ConversationTurn[];
      return fetchRecentConversation(sessionId, client, signal);
    },
    refetchInterval: () => getPollingInterval({
      isPolling,
      pollStartedAt,
      isConnected,
      consecutiveErrors: consecutiveErrorsRef.current,
    }),
  });
  const optimisticQuery = useQuery({
    queryKey: optimisticQueryKey,
    queryFn: async () => [] as ConversationTurn[],
    staleTime: Number.POSITIVE_INFINITY,
    retry: false,
    initialData: [] as ConversationTurn[],
  });

  useEffect(() => {
    if (query.isError) {
      consecutiveErrorsRef.current = Math.max(1, query.failureCount);
      return;
    }
    if (query.isSuccess) {
      consecutiveErrorsRef.current = 0;
    }
  }, [query.failureCount, query.isError, query.isSuccess]);

  // Keep legacy behavior: polling auto-stops after 60 seconds.
  useEffect(() => {
    if (!isPolling || !pollStartedAt) return;
    const remainingMs = Math.max(0, SLOW_PHASE_MS - (Date.now() - pollStartedAt));
    const timer = setTimeout(() => {
      setIsPolling(false);
      setPollStartedAt(null);
    }, remainingMs);
    return () => clearTimeout(timer);
  }, [isPolling, pollStartedAt]);

  // If connection returns during active polling, force immediate refresh.
  useEffect(() => {
    const wasConnected = prevConnectedRef.current;
    prevConnectedRef.current = isConnected;
    if (!wasConnected && isConnected && isPolling) {
      void query.refetch();
    }
  }, [isConnected, isPolling, query]);

  const startPolling = useCallback(() => {
    consecutiveErrorsRef.current = 0;
    setPollStartedAt(Date.now());
    setIsPolling(true);
    void query.refetch();
  }, [query]);

  const stopPolling = useCallback(() => {
    setIsPolling(false);
    setPollStartedAt(null);
  }, []);

  const refresh = useCallback(async () => {
    setIsRefreshing(true);
    try {
      await query.refetch();
    } finally {
      setIsRefreshing(false);
    }
  }, [query]);

  const addOptimistic = useCallback((text: string) => {
    queryClient.setQueryData<ConversationTurn[]>(optimisticQueryKey, (prev = []) => [
      ...prev,
      {
        origin: "user",
        text,
        at: new Date(),
        metadata: {},
        isOptimistic: true,
      },
    ]);
  }, [optimisticQueryKey, queryClient]);

  const removeLastOptimistic = useCallback(() => {
    queryClient.setQueryData<ConversationTurn[]>(optimisticQueryKey, (prev = []) => {
      const idx = prev.findLastIndex(t => t.isOptimistic);
      if (idx === -1) return prev;
      return [...prev.slice(0, idx), ...prev.slice(idx + 1)];
    });
  }, [optimisticQueryKey, queryClient]);

  const turns = useMemo(() => {
    const base = query.data ?? [];
    const optimisticTurns = optimisticQuery.data ?? [];
    if (!optimisticTurns.length) return base;
    return [...base, ...optimisticTurns];
  }, [optimisticQuery.data, query.data]);

  // Reconcile optimistic entries with server-confirmed turns one-for-one.
  useEffect(() => {
    const optimisticTurns = optimisticQuery.data ?? [];
    if (!query.data?.length || !optimisticTurns.length) return;
    const normalize = (s: string) => s.trim().toLowerCase();
    queryClient.setQueryData<ConversationTurn[]>(optimisticQueryKey, (prev = []) => {
      if (!prev.length) return prev;
      const remaining = [...prev];
      for (const turn of query.data) {
        const idx = remaining.findIndex(
          opt => opt.origin === turn.origin && normalize(opt.text) === normalize(turn.text),
        );
        if (idx !== -1) remaining.splice(idx, 1);
      }
      return remaining;
    });
  }, [optimisticQuery.data, optimisticQueryKey, query.data, queryClient]);

  const error = query.error instanceof Error ? query.error.message : null;

  return {
    turns,
    isLoading: query.isLoading || isRefreshing,
    isPolling,
    error,
    startPolling,
    stopPolling,
    refresh,
    addOptimistic,
    removeLastOptimistic,
    resetCache: () => {
      queryClient.removeQueries({ queryKey: conversationQueryKey(sessionId) });
      queryClient.removeQueries({ queryKey: optimisticQueryKey });
    },
  };
}
