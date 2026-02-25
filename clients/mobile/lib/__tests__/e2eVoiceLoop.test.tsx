// @vitest-environment jsdom
/**
 * E2E Voice Loop Integration Test
 *
 * Tests the full voice-to-response data flow by verifying integration between:
 * - sendVoiceCommand RPC (Connect RPC client)
 * - useConversationQuery (adaptive polling, optimistic updates)
 * - ConversationPanel (rendering user + AI bubbles)
 *
 * Validates AC1: complete voice round-trip from transcription result
 * to visible AI response in the conversation panel.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, act, waitFor, renderHook, cleanup } from "@testing-library/react";
import React from "react";
import {
  createQueryWrapper, createMockClient, makeTurns, createTestContextConsumer,
  setupAudioMocks, capsResponse, audioMeta,
  mockSendInterrupt, mockDisconnect, mockConnect,
  mockEnqueuePCM, mockStopPlayback, mockUpdateFormat, mockMarkDraining,
} from "./testHelpers";
import type { AudioControlMessage } from "../audioProtocol";

// ---------------------------------------------------------------------------
// Module mocks — order matters: vi.mock is hoisted before imports
// ---------------------------------------------------------------------------

// react-native mock: resolved via vitest.config.ts alias to shared __mocks__/react-native.ts
// react-native-reanimated mock: resolved via vitest.config.ts alias to shared __mocks__/react-native-reanimated.ts

// @/components/Themed — Text stub delegating to shared mapRNProps
vi.mock("@/components/Themed", async () => {
  const { mapRNProps } = await import("./__mocks__/react-native") as any;
  const React = await import("react");
  return {
    Text: ({ children, ...props }: any) =>
      React.createElement("span", mapRNProps(props), children),
  };
});

// @/components/useColorScheme — theme color accessor
vi.mock("@/components/useColorScheme", () => ({
  useColorScheme: () => "dark",
  useThemeColors: () => ({
    tint: "#fff",
    background: "#000",
    text: "#fff",
    surface: "#222",
    danger: "#f00",
  }),
}));

// Audio subsystem mocks (Test Suite 3: TTS trigger via AudioPlaybackContext)
// Mock functions imported from testHelpers (mockConnect, mockEnqueuePCM, etc.)
vi.mock("../useAudioWebSocket", () => ({
  useAudioWebSocket: vi.fn(),
}));
vi.mock("../useAudioPlayback", () => ({
  useAudioPlayback: vi.fn(),
}));
vi.mock("../storage", () => ({
  getTtsEnabled: vi.fn(async () => true),
  addTtsChangeListener: vi.fn(() => () => {}),
}));

// ---------------------------------------------------------------------------
// Imports (after mocks)
// ---------------------------------------------------------------------------
import { ConversationPanel } from "../../components/ConversationPanel";
import type { ConversationTurn } from "../conversationTypes";
import { useConversationQuery } from "../useConversationQuery";
import { useAudioWebSocket } from "../useAudioWebSocket";
import { useAudioPlayback } from "../useAudioPlayback";
import {
  AudioPlaybackProvider,
  useAudioPlaybackContext,
} from "../AudioPlaybackContext";
import { getTtsEnabled } from "../storage";

// ---------------------------------------------------------------------------
// Test Suite 1: Full voice → RPC → conversation → panel integration (AC1)
// ---------------------------------------------------------------------------

describe("E2E Voice Loop — Full Sequence (AC1)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("Unit: sendVoiceCommand RPC accepts correct request shape (text, sessionId, metadata)", async () => {
    // Unit-level verification of RPC request shape — validates the contract
    // between session screen and Connect RPC client, not the full integration.
    const sendVoiceCommand = vi.fn(async () => ({ accepted: true, message: "queued" }));
    const getConversation = vi.fn(async () => ({ turns: [], total: 0 }));
    const client = createMockClient(getConversation, sendVoiceCommand);

    await client.sessions.sendVoiceCommand({
      sessionId: "session-1",
      text: "what's happening in session one",
      metadata: { client_type: "mobile" },
    });

    expect(sendVoiceCommand).toHaveBeenCalledWith({
      sessionId: "session-1",
      text: "what's happening in session one",
      metadata: { client_type: "mobile" },
    });
  });

  it("useConversationQuery picks up both user and AI turns after polling", async () => {
    let serverTurns: Array<{ origin: string; text: string; metadata: Array<{ key: string; value: string }> }> = [];

    const getConversation = vi.fn(async (req: any) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns, total: serverTurns.length };
    });
    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", client, true),
      { wrapper },
    );

    await waitFor(() => expect(result.current.turns).toEqual([]));

    // Simulate server processing: user turn + AI response stored
    serverTurns = [
      { origin: "user", text: "what's happening in session one", metadata: [{ key: "input_source", value: "voice" }] },
      { origin: "ai", text: "Session one is running a build process.", metadata: [] },
    ];

    // Start polling (session screen does this after sendVoiceCommand)
    act(() => {
      result.current.startPolling();
    });

    await waitFor(() => {
      expect(result.current.turns.length).toBe(2);
    });

    expect(result.current.turns[0]?.origin).toBe("user");
    expect(result.current.turns[0]?.text).toBe("what's happening in session one");
    expect(result.current.turns[1]?.origin).toBe("ai");
    expect(result.current.turns[1]?.text).toBe("Session one is running a build process.");
  });

  it("optimistic user turn shows immediately, then reconciles with server turn", async () => {
    let serverTurns: Array<{ origin: string; text: string; metadata: Array<{ key: string; value: string }> }> = [];

    const getConversation = vi.fn(async (req: any) => {
      if (req.limit === 0) return { turns: [], total: serverTurns.length };
      return { turns: serverTurns, total: serverTurns.length };
    });
    const client = createMockClient(getConversation);
    const wrapper = createQueryWrapper();

    const { result } = renderHook(
      () => useConversationQuery("session-1", client, true),
      { wrapper },
    );
    await waitFor(() => expect(result.current.turns).toEqual([]));

    // Add optimistic turn (session screen does this before sendVoiceCommand completes)
    act(() => {
      result.current.addOptimistic("hello nupi");
    });

    await waitFor(() => {
      expect(result.current.turns.length).toBe(1);
      expect(result.current.turns[0]?.isOptimistic).toBe(true);
      expect(result.current.turns[0]?.text).toBe("hello nupi");
    });

    // Server confirms the turn
    serverTurns = [{ origin: "user", text: "hello nupi", metadata: [] }];

    await act(async () => {
      await result.current.refresh();
    });

    // Optimistic turn reconciled — replaced by server turn
    await waitFor(() => {
      expect(result.current.turns.length).toBe(1);
      expect(result.current.turns[0]?.isOptimistic).toBeUndefined();
      expect(result.current.turns[0]?.text).toBe("hello nupi");
    });
  });

  it("auto-send effect triggers sendVoiceCommand when recordingStatus transitions to 'result'", async () => {
    // Validates the trigger mechanism in session/[id].tsx (lines 444-534).
    //
    // SYNC WITH app/session/[id].tsx — this harness replicates the auto-send
    // effect's core guard logic. If the production effect changes (state machine,
    // ref guard, dependencies), update this harness to match.
    //
    // Production conditions replicated here:
    //   - recordingStatus === "result" gate
    //   - confirmedText non-empty gate
    //   - isSendingRef re-entry guard (prevents double-send)
    //   - client null gate
    //   - metadata: { client_type: "mobile" } matches production (line 486)
    //
    // Production conditions NOT replicated (require full session screen context):
    //   - voiceCommandStatus state machine (sending/thinking/error/idle)
    //   - notification permission request on first command
    //   - addOptimistic/clearTranscription side effects
    //   - raceTimeout (10s) on sendVoiceCommand RPC
    //   - timeout → start polling (fire-and-forget), error → 3s auto-recovery
    //   - pendingCommandsRef counter for AI response detection
    //   - input_source: "voice" — NOT sent by client; added daemon-side in
    //     grpc_services.go when constructing ConversationPromptEvent metadata.
    //     Client only sends { client_type: "mobile" }.
    const sendVoiceCommand = vi.fn(async () => ({ accepted: true, message: "queued" }));
    const getConversation = vi.fn(async () => ({ turns: [], total: 0 }));
    const client = createMockClient(getConversation, sendVoiceCommand);

    function AutoSendHarness({
      recordingStatus,
      confirmedText,
      sessionId,
      nupiClient,
    }: {
      recordingStatus: string;
      confirmedText: string;
      sessionId: string;
      nupiClient: typeof client | null;
    }) {
      const isSendingRef = React.useRef(false);
      React.useEffect(() => {
        if (recordingStatus !== "result" || !confirmedText || isSendingRef.current) return;
        if (!nupiClient) return;
        isSendingRef.current = true;
        nupiClient.sessions.sendVoiceCommand({
          sessionId,
          text: confirmedText,
          metadata: { client_type: "mobile" },
        });
      }, [recordingStatus, confirmedText, sessionId, nupiClient]);
      return null;
    }

    // Idle — no trigger
    const { rerender } = render(
      React.createElement(AutoSendHarness, {
        recordingStatus: "idle",
        confirmedText: "",
        sessionId: "session-1",
        nupiClient: client,
      }),
    );
    expect(sendVoiceCommand).not.toHaveBeenCalled();

    // Recording — no trigger
    rerender(
      React.createElement(AutoSendHarness, {
        recordingStatus: "recording",
        confirmedText: "",
        sessionId: "session-1",
        nupiClient: client,
      }),
    );
    expect(sendVoiceCommand).not.toHaveBeenCalled();

    // Null client — no trigger even with result status
    rerender(
      React.createElement(AutoSendHarness, {
        recordingStatus: "result",
        confirmedText: "deploy to production",
        sessionId: "session-1",
        nupiClient: null,
      }),
    );
    expect(sendVoiceCommand).not.toHaveBeenCalled();

    // Result with text and client — triggers sendVoiceCommand
    rerender(
      React.createElement(AutoSendHarness, {
        recordingStatus: "result",
        confirmedText: "deploy to production",
        sessionId: "session-1",
        nupiClient: client,
      }),
    );
    await waitFor(() => {
      expect(sendVoiceCommand).toHaveBeenCalledWith({
        sessionId: "session-1",
        text: "deploy to production",
        metadata: { client_type: "mobile" },
      });
    });

    // Re-entry guard (isSendingRef) — only fires once
    rerender(
      React.createElement(AutoSendHarness, {
        recordingStatus: "result",
        confirmedText: "deploy to production",
        sessionId: "session-1",
        nupiClient: client,
      }),
    );
    expect(sendVoiceCommand).toHaveBeenCalledTimes(1);
  });

  it("isSendingRef resets after sendVoiceCommand completes, allowing sequential voice commands", async () => {
    // Validates that the finally block in production (session/[id].tsx line 528-530)
    // resets isSendingRef, enabling a second voice command through the auto-send effect.
    // SYNC WITH app/session/[id].tsx — must include finally { isSendingRef.current = false }
    const sendVoiceCommand = vi.fn(async () => ({ accepted: true, message: "queued" }));
    const getConversation = vi.fn(async () => ({ turns: [], total: 0 }));
    const client = createMockClient(getConversation, sendVoiceCommand);

    function AutoSendWithReset({
      recordingStatus,
      confirmedText,
      sessionId,
      nupiClient,
    }: {
      recordingStatus: string;
      confirmedText: string;
      sessionId: string;
      nupiClient: typeof client | null;
    }) {
      const isSendingRef = React.useRef(false);
      React.useEffect(() => {
        if (recordingStatus !== "result" || !confirmedText || isSendingRef.current) return;
        if (!nupiClient) return;
        isSendingRef.current = true;
        (async () => {
          try {
            await nupiClient.sessions.sendVoiceCommand({
              sessionId,
              text: confirmedText,
              metadata: { client_type: "mobile" },
            });
          } finally {
            isSendingRef.current = false;
          }
        })();
      }, [recordingStatus, confirmedText, sessionId, nupiClient]);
      return null;
    }

    // First voice command
    const { rerender } = render(
      React.createElement(AutoSendWithReset, {
        recordingStatus: "result",
        confirmedText: "first command",
        sessionId: "session-1",
        nupiClient: client,
      }),
    );

    await waitFor(() => {
      expect(sendVoiceCommand).toHaveBeenCalledTimes(1);
    });

    // Reset to idle (VoiceContext clears after send)
    rerender(
      React.createElement(AutoSendWithReset, {
        recordingStatus: "idle",
        confirmedText: "",
        sessionId: "session-1",
        nupiClient: client,
      }),
    );

    // Second voice command — isSendingRef should have been reset by finally block
    rerender(
      React.createElement(AutoSendWithReset, {
        recordingStatus: "result",
        confirmedText: "second command",
        sessionId: "session-1",
        nupiClient: client,
      }),
    );

    await waitFor(() => {
      expect(sendVoiceCommand).toHaveBeenCalledTimes(2);
    });

    expect(sendVoiceCommand).toHaveBeenLastCalledWith({
      sessionId: "session-1",
      text: "second command",
      metadata: { client_type: "mobile" },
    });
  });
});

// ---------------------------------------------------------------------------
// Test Suite 2: ConversationPanel rendering (AC1, subtask 2.4)
// ---------------------------------------------------------------------------

// RN props are mapped to DOM equivalents by shared mapRNProps (testID→data-testid, etc.)
function byTestId(id: string) {
  const el = document.querySelector(`[data-testid="${id}"]`);
  if (!el) throw new Error(`Element with data-testid="${id}" not found`);
  return el;
}

function byLabel(label: string) {
  const el = document.querySelector(`[aria-label="${label}"]`);
  if (!el) throw new Error(`Element with aria-label="${label}" not found`);
  return el;
}

describe("ConversationPanel — User and AI bubbles", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("renders user bubble and AI bubble with correct text content", () => {
    const turns = makeTurns(
      { origin: "user", text: "what's happening in session one" },
      { origin: "ai", text: "Session one is running a build process." },
    );

    render(React.createElement(ConversationPanel, { turns, isThinking: false }));

    // byTestId throws if element not found — existence is already asserted
    const panel = byTestId("conversation-panel");

    const userMsg = byTestId("conversation-message-0");
    expect(userMsg.textContent).toContain("what's happening in session one");

    const aiMsg = byTestId("conversation-message-1");
    expect(aiMsg.textContent).toContain("Session one is running a build process.");
  });

  it("user bubble has 'user says:' accessibility label", () => {
    const turns = makeTurns({ origin: "user", text: "hello" });
    render(React.createElement(ConversationPanel, { turns, isThinking: false }));
    const msg = byTestId("conversation-message-0");
    const label = msg.getAttribute("aria-label") ?? "";
    expect(label).toContain("user says:");
  });

  it("AI bubble has 'ai says:' accessibility label", () => {
    const turns = makeTurns({ origin: "ai", text: "hi there" });
    render(React.createElement(ConversationPanel, { turns, isThinking: false }));
    const msg = byTestId("conversation-message-0");
    const label = msg.getAttribute("aria-label") ?? "";
    expect(label).toContain("ai says:");
  });

  it("shows thinking indicator when isThinking=true", () => {
    const turns = makeTurns({ origin: "user", text: "hello" });
    render(React.createElement(ConversationPanel, { turns, isThinking: true }));
    // byLabel throws if element not found — existence is already asserted
    byLabel("AI is thinking");
  });

  it("optimistic user bubble renders with correct text and user origin", () => {
    const turns: ConversationTurn[] = [
      { origin: "user", text: "hello", at: new Date(), metadata: {}, isOptimistic: true },
    ];
    render(React.createElement(ConversationPanel, { turns, isThinking: false }));
    const msg = byTestId("conversation-message-0");
    expect(msg.textContent).toContain("hello");
    // Verify it renders as a user bubble (accessibility label indicates origin)
    const label = msg.getAttribute("aria-label") ?? "";
    expect(label).toContain("user says:");
  });

  it("renders both send error and poll error inline", () => {
    const turns = makeTurns({ origin: "user", text: "hi" });
    render(
      React.createElement(ConversationPanel, {
        turns,
        isThinking: false,
        sendError: true,
        pollError: "Network error",
      }),
    );
    // byTestId throws if element not found — existence is already asserted
    byTestId("conversation-send-error");
    byTestId("conversation-poll-error");
  });

  it("renders 'today' time format for turns created today", () => {
    // Use fake timers with an arbitrary fixed date to prevent midnight race
    // condition (live new Date() could cross midnight between turn creation
    // and assertion). The specific date value is irrelevant — only matters
    // that it's "today" relative to the fake clock.
    const fixedNow = new Date(2026, 1, 25, 14, 30, 0);
    vi.useFakeTimers({ now: fixedNow });
    try {
      const turns: ConversationTurn[] = [
        { origin: "user", text: "hello today", at: fixedNow, metadata: {} },
      ];
      render(React.createElement(ConversationPanel, { turns, isThinking: false }));
      const msg = byTestId("conversation-message-0");
      // formatTime uses manual padStart (not toLocaleTimeString), so "14:30"
      // is deterministic across all locales — no locale dependency risk.
      expect(msg.textContent).toContain("14:30");
      // Must NOT contain date prefix (dd/mm) for today's turns
      expect(msg.textContent).not.toContain("25/02 14:30");
    } finally {
      vi.useRealTimers();
    }
  });
});

// ---------------------------------------------------------------------------
// Test Suite 3: TTS trigger via AudioPlaybackContext (AC1, subtask 2.3)
// ---------------------------------------------------------------------------

/** Local wrapper — delegates to shared setupAudioMocks with this file's mocked hooks. */
function setupLocalAudioMocks(overrides?: { wsStatus?: string; playbackState?: string }) {
  return setupAudioMocks({ useAudioWebSocket, useAudioPlayback }, overrides);
}

const TestContextConsumer = createTestContextConsumer(useAudioPlaybackContext);

describe("TTS trigger via AudioPlaybackContext (AC1, subtask 2.3)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("audio_meta → PCM frame → enqueuePCM called (TTS playback triggered)", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
      wsStatus: "connected",
    });

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestContextConsumer onValues={() => {}} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Establish TTS capability
    act(() => {
      capturedOnControlMessage()({
        type: "capabilities_response",
        capture: [],
        playback: [{ stream_id: "tts", ready: true }],
      });
    });

    // Receive audio_meta — unlocks PCM buffering
    act(() => {
      capturedOnControlMessage()({
        type: "audio_meta",
        format: { encoding: "pcm_s16le", sample_rate: 24000, channels: 1, bit_depth: 16 },
        sequence: 1,
        first: true,
        last: false,
        duration_ms: 20,
      });
    });

    expect(mockUpdateFormat).toHaveBeenCalledWith(
      expect.objectContaining({ sample_rate: 24000 }),
    );

    // Send PCM frame — should be enqueued (not buffered)
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(480));
    });

    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("end-of-stream audio_meta (last=true) calls markDraining", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
      wsStatus: "connected",
    });

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestContextConsumer onValues={() => {}} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    act(() => {
      capturedOnControlMessage()({
        type: "capabilities_response",
        capture: [],
        playback: [{ stream_id: "tts", ready: true }],
      });
    });

    // First audio_meta unlocks buffering
    act(() => {
      capturedOnControlMessage()({
        type: "audio_meta",
        format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 },
        sequence: 1,
        first: true,
        last: false,
        duration_ms: 20,
      });
    });

    // PCM flows through
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(320));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // End-of-stream
    act(() => {
      capturedOnControlMessage()({
        type: "audio_meta",
        format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 },
        sequence: 2,
        first: false,
        last: true,
        duration_ms: 20,
      });
    });

    expect(mockMarkDraining).toHaveBeenCalled();
  });

  it("audio_meta.last resets state correctly: next PCM is buffered, next audio_meta flushes", async () => {
    // Verifies the audio_meta.last → new stream transition: after end-of-stream,
    // awaitingFirstMeta is true so subsequent PCM frames are buffered (not directly
    // enqueued). When the next stream's audio_meta arrives, buffered frames flush.
    // This mirrors close_stream state reset but via the audio_meta.last path.
    const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
      wsStatus: "connected",
    });

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestContextConsumer onValues={() => {}} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Establish TTS capability
    act(() => {
      capturedOnControlMessage()(capsResponse(["tts"]));
    });

    // First stream: normal playback
    act(() => {
      capturedOnControlMessage()(audioMeta({ first: true }));
    });
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(320));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // End first stream with audio_meta.last
    mockEnqueuePCM.mockClear();
    mockMarkDraining.mockClear();
    act(() => {
      capturedOnControlMessage()(audioMeta({ last: true }));
    });
    expect(mockMarkDraining).toHaveBeenCalled();

    // PCM arriving after audio_meta.last is buffered (awaitingFirstMeta reset to true)
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    // New stream's audio_meta(first=true) discards buffered old-stream PCM
    // and resets for clean playback (same as close_stream → new stream path)
    act(() => {
      capturedOnControlMessage()(audioMeta({ first: true }));
    });

    // New PCM after audio_meta flows through directly
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(320));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("close_stream triggers markDraining and resets PCM buffering for next stream", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
      wsStatus: "connected",
    });

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestContextConsumer onValues={() => {}} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Establish TTS capability
    act(() => {
      capturedOnControlMessage()(capsResponse(["tts"]));
    });

    // Receive audio_meta + PCM — normal playback
    act(() => {
      capturedOnControlMessage()(audioMeta({ first: true }));
    });
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(320));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // Server sends close_stream — graceful termination (distinct from audio_meta.last)
    act(() => {
      capturedOnControlMessage()({ type: "close_stream" } as AudioControlMessage);
    });

    // markDraining called — lets queued buffers play out
    expect(mockMarkDraining).toHaveBeenCalled();

    // awaitingFirstMeta reset — next PCM frames are buffered until new audio_meta arrives
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    // New stream's audio_meta(first=true) discards buffered PCM from the closed stream
    // (AudioPlaybackContext line 199-201: first=true clears pending buffer to prevent
    // old stream frames leaking into new stream playback).
    act(() => {
      capturedOnControlMessage()(audioMeta({ first: true }));
    });

    // Old PCM discarded — new PCM frames after audio_meta flow through directly
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(320));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("close_stream while fallback timer is active: cancels timer and clears stale buffer", async () => {
    // Edge case: PCM frames arrive WITHOUT audio_meta (starts 2s fallback timer),
    // then close_stream arrives before timer fires. The timer must be cancelled
    // and pending buffer cleared to prevent stale frames leaking into the next stream.
    vi.useFakeTimers();
    try {
      const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
        wsStatus: "connected",
      });

      render(
        <AudioPlaybackProvider sessionId="s1">
          <TestContextConsumer onValues={() => {}} />
        </AudioPlaybackProvider>,
      );

      await act(async () => { await Promise.resolve(); });

      // Establish TTS capability
      act(() => {
        capturedOnControlMessage()(capsResponse(["tts"]));
      });

      // Send PCM without audio_meta — starts 2s fallback timer, buffers frames
      act(() => {
        capturedOnPcmData()(new ArrayBuffer(160));
        capturedOnPcmData()(new ArrayBuffer(160));
      });
      expect(mockEnqueuePCM).not.toHaveBeenCalled();

      // close_stream arrives BEFORE the 2s timer fires
      act(() => {
        capturedOnControlMessage()({ type: "close_stream" } as AudioControlMessage);
      });
      expect(mockMarkDraining).toHaveBeenCalled();

      // Advance past where the 2s timer WOULD have fired
      await act(async () => {
        vi.advanceTimersByTime(3000);
      });

      // Timer was cancelled by close_stream — stale frames NOT flushed
      expect(mockEnqueuePCM).not.toHaveBeenCalled();

      // New stream works normally: audio_meta → PCM flows through
      mockMarkDraining.mockClear();
      act(() => {
        capturedOnControlMessage()(audioMeta({ first: true }));
      });
      act(() => {
        capturedOnPcmData()(new ArrayBuffer(320));
      });
      expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("PCM frames are buffered until audio_meta arrives, then flushed", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
      wsStatus: "connected",
    });

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestContextConsumer onValues={() => {}} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    act(() => {
      capturedOnControlMessage()({
        type: "capabilities_response",
        capture: [],
        playback: [{ stream_id: "tts", ready: true }],
      });
    });

    // Send PCM BEFORE audio_meta — should be buffered
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    // audio_meta arrives — flushes buffered frames
    act(() => {
      capturedOnControlMessage()({
        type: "audio_meta",
        format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 },
        sequence: 1,
        first: false,
        last: false,
        duration_ms: 20,
      });
    });

    expect(mockEnqueuePCM).toHaveBeenCalledTimes(2);
  });

  it("PCM buffer cap (500 frames) drops buffer when exceeded without audio_meta", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
      wsStatus: "connected",
    });

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestContextConsumer onValues={() => {}} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    act(() => {
      capturedOnControlMessage()({
        type: "capabilities_response",
        capture: [],
        playback: [{ stream_id: "tts", ready: true }],
      });
    });

    // Send 501 PCM frames without audio_meta — exceeds MAX_PENDING_PCM_FRAMES (500).
    // At frame 500 the buffer is dropped (cleared), frame 501 re-buffered as first entry.
    act(() => {
      for (let i = 0; i < 501; i++) {
        capturedOnPcmData()(new ArrayBuffer(160));
      }
    });

    // Nothing enqueued yet — all frames were buffered/dropped
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    // audio_meta arrives — flushes only the 1 frame remaining after overflow drop
    act(() => {
      capturedOnControlMessage()({
        type: "audio_meta",
        format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 },
        sequence: 1,
        first: false,
        last: false,
        duration_ms: 20,
      });
    });

    // Only 1 frame survived the buffer cap reset (the 501st)
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("PCM buffer auto-flushes after 2s fallback if audio_meta never arrives", async () => {
    vi.useFakeTimers();
    try {
      const { capturedOnControlMessage, capturedOnPcmData } = setupLocalAudioMocks({
        wsStatus: "connected",
      });

      render(
        <AudioPlaybackProvider sessionId="s1">
          <TestContextConsumer onValues={() => {}} />
        </AudioPlaybackProvider>,
      );

      await act(async () => { await Promise.resolve(); });

      act(() => {
        capturedOnControlMessage()({
          type: "capabilities_response",
          capture: [],
          playback: [{ stream_id: "tts", ready: true }],
        });
      });

      // Send PCM without audio_meta — starts 2s fallback timer
      act(() => {
        capturedOnPcmData()(new ArrayBuffer(160));
        capturedOnPcmData()(new ArrayBuffer(160));
        capturedOnPcmData()(new ArrayBuffer(160));
      });
      expect(mockEnqueuePCM).not.toHaveBeenCalled();
      expect(mockUpdateFormat).not.toHaveBeenCalled();

      // Advance past 2s fallback timer
      await act(async () => {
        vi.advanceTimersByTime(2100);
      });

      // Buffer auto-flushed without audio_meta — enqueuePCM called for each buffered frame
      expect(mockEnqueuePCM).toHaveBeenCalledTimes(3);
      // updateFormat NOT called — no audio_meta arrived, plays at default format
      expect(mockUpdateFormat).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("audio WebSocket does NOT connect while userTtsEnabled is null (storage not yet loaded)", async () => {
    // AudioPlaybackProvider initializes userTtsEnabled as null (not yet loaded from
    // getTtsEnabled). The connect effect guards with `userTtsEnabled === true`, so
    // during the null window the audio WS must NOT attempt connection. This prevents
    // a race condition where WS connects before the user's TTS preference is known.
    let resolveTtsEnabled: (val: boolean) => void;
    const pendingTtsEnabled = new Promise<boolean>((resolve) => {
      resolveTtsEnabled = resolve;
    });
    vi.mocked(getTtsEnabled).mockReturnValue(pendingTtsEnabled);

    const { capturedOnControlMessage } = setupLocalAudioMocks({
      wsStatus: "idle",
    });

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestContextConsumer onValues={() => {}} />
      </AudioPlaybackProvider>,
    );

    // Flush initial render — getTtsEnabled is still pending (userTtsEnabled === null)
    await act(async () => { await Promise.resolve(); });

    // mockConnect should NOT have been called — WS stays idle while pref is unknown
    expect(mockConnect).not.toHaveBeenCalled();

    // Now resolve the TTS preference — userTtsEnabled transitions null → true
    await act(async () => {
      resolveTtsEnabled!(true);
      await Promise.resolve();
      await Promise.resolve();
    });

    // After pref loaded with TTS enabled, WS connection should be attempted
    expect(mockConnect).toHaveBeenCalled();
  });
});
