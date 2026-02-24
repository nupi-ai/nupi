import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import type { NupiClient } from "./connect";
import type { ConversationTurn as ProtoTurn } from "./gen/sessions_pb";

export interface ConversationTurn {
  origin: "user" | "ai" | "system" | "tool";
  text: string;
  at: Date;
  metadata: Record<string, string>;
  isOptimistic?: boolean;
}

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

export function useConversationQuery(
  sessionId: string,
  client: NupiClient | null,
  isConnected = true,
) {
  const queryClient = useQueryClient();
  const [isPolling, setIsPolling] = useState(false);
  const [pollStartedAt, setPollStartedAt] = useState<number | null>(null);
  const [optimisticTurns, setOptimisticTurns] = useState<ConversationTurn[]>([]);
  const consecutiveErrorsRef = useRef(0);

  const query = useQuery({
    queryKey: conversationQueryKey(sessionId),
    enabled: Boolean(client),
    queryFn: async ({ signal }) => {
      if (!client) return [] as ConversationTurn[];
      return fetchRecentConversation(sessionId, client, signal);
    },
    refetchInterval: () => {
      if (!isPolling || !pollStartedAt || !isConnected) return false;
      const elapsed = Date.now() - pollStartedAt;
      if (elapsed >= SLOW_PHASE_MS) return false;
      if (consecutiveErrorsRef.current >= 3) return SLOW_INTERVAL;
      return elapsed < FAST_PHASE_MS ? FAST_INTERVAL : SLOW_INTERVAL;
    },
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
    await query.refetch();
  }, [query]);

  const addOptimistic = useCallback((text: string) => {
    setOptimisticTurns(prev => [
      ...prev,
      {
        origin: "user",
        text,
        at: new Date(),
        metadata: {},
        isOptimistic: true,
      },
    ]);
  }, []);

  const removeLastOptimistic = useCallback(() => {
    setOptimisticTurns(prev => {
      const idx = prev.findLastIndex(t => t.isOptimistic);
      if (idx === -1) return prev;
      return [...prev.slice(0, idx), ...prev.slice(idx + 1)];
    });
  }, []);

  const turns = useMemo(() => {
    const base = query.data ?? [];
    if (!optimisticTurns.length) return base;
    return [...base, ...optimisticTurns];
  }, [query.data, optimisticTurns]);

  const error = query.error instanceof Error ? query.error.message : null;

  return {
    turns,
    isLoading: query.isLoading || query.isFetching,
    isPolling,
    error,
    startPolling,
    stopPolling,
    refresh,
    addOptimistic,
    removeLastOptimistic,
    resetCache: () => queryClient.removeQueries({ queryKey: conversationQueryKey(sessionId) }),
  };
}
