// @vitest-environment jsdom
/**
 * Story 11-5, Task 5: Validate simultaneous desktop and mobile usage (AC4)
 *
 * Tests that conversation state is session-scoped and accessible from any client.
 * Validates:
 * - 5.2: GetConversation returns turns for a specific session_id regardless of which client triggered them
 * - 5.3: Conversation polling picks up turns regardless of origin client
 * - 5.4: No client-side session locking or single-client assumptions in useConversationQuery
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";

import { useConversationQuery, conversationQueryKey } from "../useConversationQuery";
import { createQueryWrapper, createMockClient } from "./testHelpers";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/**
 * Proto-format turn as returned by the raw getConversation RPC.
 * Metadata is an array of `{ key, value }` pairs — matching the protobuf wire format.
 * `useConversationQuery.mapProtoTurn()` converts this to `Record<string, string>`,
 * so assertions like `turns[0].metadata.input_source` work after the hook transforms data.
 *
 * Contrast with `makeTurns()` in testHelpers.ts which returns post-transform format
 * (metadata as flat object) for component-level tests.
 */
type MockProtoTurn = {
  origin: string;
  text: string;
  metadata: Array<{ key: string; value: string }>;
};

// ---------------------------------------------------------------------------
// 5.2: Conversation is session-scoped
// ---------------------------------------------------------------------------

describe("5.2 — Conversation is session-scoped (AC4)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("GetConversation returns turns for the given session_id including turns from different clients", async () => {
    // Server stores turns from both mobile voice and desktop text for the same session
    const sessionTurns: MockProtoTurn[] = [
      { origin: "user", text: "show logs", metadata: [{ key: "input_source", value: "mobile_voice" }] },
      { origin: "ai", text: "Here are the recent logs.", metadata: [] },
      { origin: "user", text: "restart service", metadata: [{ key: "input_source", value: "desktop_text" }] },
      { origin: "ai", text: "Service restarted.", metadata: [] },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      expect(req.sessionId).toBe("shared-session-1");
      if (req.limit === 0) return { turns: [], total: sessionTurns.length };
      return { turns: sessionTurns.slice(req.offset, req.offset + req.limit), total: sessionTurns.length };
    });

    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("shared-session-1", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isLoading).toBe(false));

    // All four turns should be visible regardless of origin client
    expect(result.current.turns).toHaveLength(4);
    expect(result.current.turns[0]?.text).toBe("show logs");
    expect(result.current.turns[0]?.metadata.input_source).toBe("mobile_voice");
    expect(result.current.turns[2]?.text).toBe("restart service");
    expect(result.current.turns[2]?.metadata.input_source).toBe("desktop_text");
  });

  it("different session_ids return independent turn sets (no cross-session leaking)", async () => {
    const session1Turns: MockProtoTurn[] = [
      { origin: "user", text: "session 1 turn", metadata: [{ key: "input_source", value: "mobile_voice" }] },
    ];
    const session2Turns: MockProtoTurn[] = [
      { origin: "user", text: "session 2 turn", metadata: [{ key: "input_source", value: "desktop_text" }] },
      { origin: "ai", text: "response for session 2", metadata: [] },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      const turns = req.sessionId === "session-1" ? session1Turns : session2Turns;
      if (req.limit === 0) return { turns: [], total: turns.length };
      return { turns: turns.slice(req.offset, req.offset + req.limit), total: turns.length };
    });

    const wrapper = createQueryWrapper();

    const { result: result1 } = renderHook(
      () => useConversationQuery("session-1", createMockClient(getConversation), true),
      { wrapper },
    );
    const { result: result2 } = renderHook(
      () => useConversationQuery("session-2", createMockClient(getConversation), true),
      { wrapper },
    );

    await waitFor(() => {
      expect(result1.current.isLoading).toBe(false);
      expect(result2.current.isLoading).toBe(false);
    });

    expect(result1.current.turns).toHaveLength(1);
    expect(result1.current.turns[0]?.text).toBe("session 1 turn");

    expect(result2.current.turns).toHaveLength(2);
    expect(result2.current.turns[0]?.text).toBe("session 2 turn");
  });

  it("metadata preserving input_source is mapped correctly for both mobile and desktop origins", async () => {
    const sessionTurns: MockProtoTurn[] = [
      {
        origin: "user",
        text: "voice command",
        metadata: [
          { key: "input_source", value: "mobile_voice" },
          { key: "client_type", value: "mobile" },
        ],
      },
      {
        origin: "user",
        text: "typed command",
        metadata: [
          { key: "input_source", value: "desktop_text" },
          { key: "client_type", value: "desktop" },
        ],
      },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      if (req.limit === 0) return { turns: [], total: sessionTurns.length };
      return { turns: sessionTurns, total: sessionTurns.length };
    });

    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-x", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isLoading).toBe(false));

    const [mobileVoiceTurn, desktopTextTurn] = result.current.turns;
    expect(mobileVoiceTurn?.metadata.input_source).toBe("mobile_voice");
    expect(mobileVoiceTurn?.metadata.client_type).toBe("mobile");
    expect(desktopTextTurn?.metadata.input_source).toBe("desktop_text");
    expect(desktopTextTurn?.metadata.client_type).toBe("desktop");
  });
});

// ---------------------------------------------------------------------------
// 5.3: Conversation polling picks up turns regardless of origin client
// ---------------------------------------------------------------------------

describe("5.3 — Polling picks up turns regardless of origin client (AC4)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("polling picks up a mobile voice user turn and its AI response", async () => {
    let serverTurns: MockProtoTurn[] = [];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns.slice(req.offset, req.offset + req.limit), total: serverTurns.length };
    });

    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.turns).toEqual([]));

    // Simulate: mobile voice command processed server-side, adds user + AI turns
    serverTurns = [
      { origin: "user", text: "deploy to staging", metadata: [{ key: "input_source", value: "mobile_voice" }] },
      { origin: "ai", text: "Deploying to staging environment now.", metadata: [] },
    ];

    act(() => {
      result.current.startPolling();
    });

    await waitFor(() => {
      expect(result.current.turns).toHaveLength(2);
    });

    expect(result.current.turns[0]?.origin).toBe("user");
    expect(result.current.turns[0]?.metadata.input_source).toBe("mobile_voice");
    expect(result.current.turns[1]?.origin).toBe("ai");
    expect(result.current.turns[1]?.text).toBe("Deploying to staging environment now.");
  });

  it("polling picks up desktop-originated turns appearing after mobile-originated turns", async () => {
    let serverTurns: MockProtoTurn[] = [
      { origin: "user", text: "check status", metadata: [{ key: "input_source", value: "mobile_voice" }] },
      { origin: "ai", text: "All services are running.", metadata: [] },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns.slice(req.offset, req.offset + req.limit), total: serverTurns.length };
    });

    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.turns).toHaveLength(2));

    // Desktop user adds a command after mobile user's interaction
    serverTurns = [
      ...serverTurns,
      { origin: "user", text: "scale to 3 replicas", metadata: [{ key: "input_source", value: "desktop_text" }] },
      { origin: "ai", text: "Scaling to 3 replicas.", metadata: [] },
    ];

    await act(async () => {
      await result.current.refresh();
    });

    await waitFor(() => {
      expect(result.current.turns).toHaveLength(4);
    });

    expect(result.current.turns[2]?.text).toBe("scale to 3 replicas");
    expect(result.current.turns[2]?.metadata.input_source).toBe("desktop_text");
    expect(result.current.turns[3]?.text).toBe("Scaling to 3 replicas.");
  });

  it("interleaved multi-client turns are returned in server order", async () => {
    const serverTurns: MockProtoTurn[] = [
      { origin: "user", text: "mobile turn 1", metadata: [{ key: "input_source", value: "mobile_voice" }] },
      { origin: "ai", text: "response 1", metadata: [] },
      { origin: "user", text: "desktop turn 1", metadata: [{ key: "input_source", value: "desktop_text" }] },
      { origin: "ai", text: "response 2", metadata: [] },
      { origin: "user", text: "mobile turn 2", metadata: [{ key: "input_source", value: "mobile_voice" }] },
      { origin: "ai", text: "response 3", metadata: [] },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns.slice(req.offset, req.offset + req.limit), total: serverTurns.length };
    });

    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.turns).toHaveLength(6));

    // Verify all turns arrive in server order with correct metadata
    expect(result.current.turns[0]?.text).toBe("mobile turn 1");
    expect(result.current.turns[0]?.metadata.input_source).toBe("mobile_voice");
    expect(result.current.turns[2]?.text).toBe("desktop turn 1");
    expect(result.current.turns[2]?.metadata.input_source).toBe("desktop_text");
    expect(result.current.turns[4]?.text).toBe("mobile turn 2");
    expect(result.current.turns[4]?.metadata.input_source).toBe("mobile_voice");
  });

  it("polling detects new turns added by another client while polling is active", async () => {
    let serverTurns: MockProtoTurn[] = [];
    let fetchCount = 0;

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      fetchCount++;
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns.slice(req.offset, req.offset + req.limit), total: serverTurns.length };
    });

    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.turns).toEqual([]));

    // Start polling: simulates mobile waiting for response
    act(() => {
      result.current.startPolling();
    });

    const callsAfterStart = fetchCount;

    // Meanwhile, desktop client sends a command that the server processes
    serverTurns = [
      { origin: "user", text: "run tests", metadata: [{ key: "input_source", value: "desktop_text" }] },
      { origin: "ai", text: "Running test suite...", metadata: [] },
    ];

    // Polling should pick up the desktop-originated turns
    await waitFor(() => {
      expect(result.current.turns).toHaveLength(2);
    });

    expect(result.current.turns[0]?.text).toBe("run tests");
    expect(result.current.turns[0]?.metadata.input_source).toBe("desktop_text");
    // Confirm getConversation was called more than once (polling is active)
    expect(fetchCount).toBeGreaterThan(callsAfterStart);
  });
});

// ---------------------------------------------------------------------------
// 5.4: No session locking or single-client assumptions
// ---------------------------------------------------------------------------

describe("5.4 — No session locking or single-client assumptions (AC4)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("two hook instances for the same session share query data via react-query cache", async () => {
    const serverTurns: MockProtoTurn[] = [
      { origin: "user", text: "hello", metadata: [{ key: "input_source", value: "mobile_voice" }] },
      { origin: "ai", text: "hi there", metadata: [] },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns, total: serverTurns.length };
    });

    // Same wrapper (same QueryClient) simulates two components viewing the same session
    const wrapper = createQueryWrapper();

    const { result: hookA } = renderHook(
      () => useConversationQuery("session-1", createMockClient(getConversation), true),
      { wrapper },
    );
    const { result: hookB } = renderHook(
      () => useConversationQuery("session-1", createMockClient(getConversation), true),
      { wrapper },
    );

    await waitFor(() => {
      expect(hookA.current.isLoading).toBe(false);
      expect(hookB.current.isLoading).toBe(false);
    });

    // Both hooks see the same turns — no locking or exclusive access
    expect(hookA.current.turns).toHaveLength(2);
    expect(hookB.current.turns).toHaveLength(2);
    expect(hookA.current.turns[0]?.text).toBe("hello");
    expect(hookB.current.turns[0]?.text).toBe("hello");
  });

  it("multiple hook instances can independently start/stop polling without blocking each other", async () => {
    let serverTurns: MockProtoTurn[] = [];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns.slice(req.offset, req.offset + req.limit), total: serverTurns.length };
    });

    const wrapper = createQueryWrapper();

    const { result: hookA } = renderHook(
      () => useConversationQuery("session-1", createMockClient(getConversation), true),
      { wrapper },
    );
    const { result: hookB } = renderHook(
      () => useConversationQuery("session-1", createMockClient(getConversation), true),
      { wrapper },
    );

    await waitFor(() => {
      expect(hookA.current.isLoading).toBe(false);
      expect(hookB.current.isLoading).toBe(false);
    });

    // Hook A starts polling
    act(() => {
      hookA.current.startPolling();
    });
    expect(hookA.current.isPolling).toBe(true);
    // Hook B's polling state is independent
    expect(hookB.current.isPolling).toBe(false);

    // Hook B starts polling independently
    act(() => {
      hookB.current.startPolling();
    });
    expect(hookB.current.isPolling).toBe(true);

    // Stop A — should not affect B
    act(() => {
      hookA.current.stopPolling();
    });
    expect(hookA.current.isPolling).toBe(false);
    expect(hookB.current.isPolling).toBe(true);
  });

  it("hook does not enforce any client identifier or lock on the session", async () => {
    const serverTurns: MockProtoTurn[] = [
      { origin: "user", text: "command from anywhere", metadata: [] },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      // Verify: getConversation is called with sessionId only — no client_id or lock token
      expect(req).toEqual(expect.objectContaining({ sessionId: "session-1" }));
      expect(req).not.toHaveProperty("clientId");
      expect(req).not.toHaveProperty("lockToken");
      expect(req).not.toHaveProperty("client_id");
      expect(req).not.toHaveProperty("lock_token");
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns, total: serverTurns.length };
    });

    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isLoading).toBe(false));

    expect(result.current.turns).toHaveLength(1);
    // getConversation was called (probe + data fetch) with no locking params
    expect(getConversation).toHaveBeenCalled();
  });

  it("refresh from one hook updates data visible to all hooks on the same session", async () => {
    let serverTurns: MockProtoTurn[] = [
      { origin: "user", text: "initial", metadata: [] },
    ];

    const getConversation = vi.fn(async (req: { sessionId: string; offset: number; limit: number }) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns.slice(req.offset, req.offset + req.limit), total: serverTurns.length };
    });

    const wrapper = createQueryWrapper();
    const mockClient = createMockClient(getConversation);

    const { result: hookA } = renderHook(
      () => useConversationQuery("session-1", mockClient, true),
      { wrapper },
    );
    const { result: hookB } = renderHook(
      () => useConversationQuery("session-1", mockClient, true),
      { wrapper },
    );

    await waitFor(() => {
      expect(hookA.current.turns).toHaveLength(1);
      expect(hookB.current.turns).toHaveLength(1);
    });

    // Desktop adds a turn; mobile hook A refreshes
    serverTurns = [
      { origin: "user", text: "initial", metadata: [] },
      { origin: "user", text: "desktop follow-up", metadata: [{ key: "input_source", value: "desktop_text" }] },
    ];

    await act(async () => {
      await hookA.current.refresh();
    });

    // Both hooks should see the updated data since they share the react-query cache
    await waitFor(() => {
      expect(hookA.current.turns).toHaveLength(2);
      expect(hookB.current.turns).toHaveLength(2);
    });

    expect(hookB.current.turns[1]?.text).toBe("desktop follow-up");
    expect(hookB.current.turns[1]?.metadata.input_source).toBe("desktop_text");
  });

  it("conversation query key is session-scoped, not client-scoped", () => {
    // Verify the query key factory produces keys based solely on sessionId
    const key1 = conversationQueryKey("session-abc");
    const key2 = conversationQueryKey("session-abc");
    const key3 = conversationQueryKey("session-xyz");

    // Same session produces identical keys (no client-specific component)
    expect(key1).toEqual(key2);
    expect(key1).toEqual(["conversation", "session-abc"]);

    // Different session produces different keys
    expect(key3).toEqual(["conversation", "session-xyz"]);
    expect(key1).not.toEqual(key3);
  });
});
