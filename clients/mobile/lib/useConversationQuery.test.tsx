// @vitest-environment jsdom

import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { NupiClient } from "./connect";
import { useConversationQuery } from "./useConversationQuery";

type MockTurn = {
  origin: string;
  text: string;
  metadata: Array<{ key: string; value: string }>;
};

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        gcTime: 0,
      },
    },
  });
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

function createClient(getConversation: (req: { sessionId: string; offset: number; limit: number }) => Promise<{ turns: MockTurn[]; total: number }>) {
  return {
    sessions: {
      getConversation,
      getGlobalConversation: vi.fn(async () => ({ turns: [], total: 0 })),
    },
  } as unknown as NupiClient;
}

describe("useConversationQuery", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("auto-stops polling after 60 seconds", async () => {
    const getConversation = vi.fn(async () => ({ turns: [], total: 0 }));
    const wrapper = createWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", createClient(getConversation), true),
      { wrapper },
    );
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    vi.useFakeTimers();
    act(() => {
      result.current.startPolling();
    });
    expect(result.current.isPolling).toBe(true);

    act(() => {
      vi.advanceTimersByTime(60_001);
    });

    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.isPolling).toBe(false);
  });

  it("reconciles optimistic turn after server confirmation", async () => {
    let serverTurns: MockTurn[] = [];
    const getConversation = vi.fn(
      async (req: { sessionId: string; offset: number; limit: number }) => {
        if (req.limit === 0) return { turns: [], total: serverTurns.length };
        return { turns: serverTurns.slice(req.offset, req.offset + req.limit), total: serverTurns.length };
      },
    );
    const wrapper = createWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", createClient(getConversation), true),
      { wrapper },
    );
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    act(() => {
      result.current.addOptimistic("Hello");
    });
    await waitFor(() => {
      expect(result.current.turns).toHaveLength(1);
      expect(result.current.turns[0]?.isOptimistic).toBe(true);
    });

    serverTurns = [{ origin: "user", text: "hello", metadata: [] }];
    await act(async () => {
      await result.current.refresh();
    });

    await waitFor(() => {
      expect(result.current.turns).toHaveLength(1);
      expect(result.current.turns[0]?.text).toBe("hello");
      expect(result.current.turns[0]?.isOptimistic).toBeUndefined();
    });
  });
});
