// @vitest-environment jsdom

import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { NupiClient } from "./connect";
import { useConversation } from "./useConversation";

type ConversationResponse = {
  turns: Array<{
    origin: string;
    text: string;
    metadata: Array<{ key: string; value: string }>;
    at?: unknown;
  }>;
  total: number;
};

function createClient(
  getConversation: (
    req: { sessionId: string; offset: number; limit: number },
    options?: { signal?: AbortSignal },
  ) => Promise<ConversationResponse>,
): NupiClient {
  return {
    sessions: {
      getConversation,
      getGlobalConversation: vi.fn(async () => ({ turns: [], total: 0 })),
    },
  } as unknown as NupiClient;
}

describe("useConversation", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("aborts in-flight poll and ignores late result after stopPolling", async () => {
    let resolvePoll: ((value: ConversationResponse) => void) | null = null;
    const getConversation = vi.fn<
      (req: { sessionId: string; offset: number; limit: number }, options?: { signal?: AbortSignal }) => Promise<ConversationResponse>
    >(async () => ({ turns: [], total: 0 }));
    const client = createClient(getConversation);

    const { result } = renderHook(() => useConversation("session-1", client, true));
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    getConversation.mockImplementation(
      async (req: { sessionId: string; offset: number; limit: number }) => {
        if (req.limit !== 10) return { turns: [], total: 0 };
        return new Promise<ConversationResponse>((resolve) => {
          resolvePoll = resolve;
        });
      },
    );

    act(() => {
      result.current.startPolling();
    });
    await act(async () => {
      await new Promise(resolve => setTimeout(resolve, 600));
    });
    expect(resolvePoll).not.toBeNull();

    act(() => {
      result.current.stopPolling();
    });
    expect(result.current.isPolling).toBe(false);

    act(() => {
      resolvePoll?.({
        turns: [{ origin: "ai", text: "late", metadata: [] }],
        total: 1,
      });
    });

    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.turns).toHaveLength(0);
  });

  it("keeps only latest refresh result when prior refresh is aborted", async () => {
    const getConversation = vi.fn<
      (req: { sessionId: string; offset: number; limit: number }, options?: { signal?: AbortSignal }) => Promise<ConversationResponse>
    >(async () => ({ turns: [], total: 0 }));
    const client = createClient(getConversation);

    const { result } = renderHook(() => useConversation("session-1", client, true));
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    let refreshProbeCount = 0;
    getConversation.mockImplementation(
      async (
        req: { sessionId: string; offset: number; limit: number },
        options?: { signal?: AbortSignal },
      ) => {
        if (req.limit === 0) {
          refreshProbeCount += 1;
          if (refreshProbeCount === 1) {
            return new Promise<ConversationResponse>((_resolve, reject) => {
              options?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
            });
          }
          return { turns: [], total: 1 };
        }
        if (req.limit === 20) {
          return { turns: [{ origin: "ai", text: "fresh", metadata: [] }], total: 1 };
        }
        return { turns: [], total: 0 };
      },
    );

    act(() => {
      void result.current.refresh();
      void result.current.refresh();
    });

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
      expect(result.current.turns).toHaveLength(1);
      expect(result.current.turns[0]?.text).toBe("fresh");
    });
  });

  it("can restart polling immediately after stop while previous poll is still in flight", async () => {
    let pollCallCount = 0;
    let resolveFirstPoll: (() => void) | null = null;
    const getConversation = vi.fn<
      (req: { sessionId: string; offset: number; limit: number }, options?: { signal?: AbortSignal }) => Promise<ConversationResponse>
    >(async () => ({ turns: [], total: 0 }));
    const client = createClient(getConversation);

    const { result } = renderHook(() => useConversation("session-1", client, true));
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    getConversation.mockImplementation(async (req) => {
      if (req.limit !== 10) return { turns: [], total: 0 };
      pollCallCount += 1;
      if (pollCallCount === 1) {
        return new Promise<ConversationResponse>((resolve) => {
          resolveFirstPoll = () => resolve({ turns: [], total: 0 });
        });
      }
      return { turns: [], total: 0 };
    });

    act(() => {
      result.current.startPolling();
    });
    await act(async () => {
      await new Promise(resolve => setTimeout(resolve, 600));
    });
    expect(pollCallCount).toBe(1);

    act(() => {
      result.current.stopPolling();
      result.current.startPolling();
    });
    act(() => {
      resolveFirstPoll?.();
    });

    await act(async () => {
      await new Promise(resolve => setTimeout(resolve, 700));
    });
    expect(pollCallCount).toBeGreaterThanOrEqual(2);
  });
});
