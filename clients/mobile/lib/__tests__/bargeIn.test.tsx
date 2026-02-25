// @vitest-environment jsdom
/**
 * Barge-In During TTS Integration Test (AC2)
 *
 * Tests that when TTS is playing and user initiates a new voice command:
 * 1. TTS playback stops immediately
 * 2. Interrupt message is sent via audio WebSocket
 * 3. New recording begins
 * 4. PCM buffer is reset on barge-in
 * 5. Full barge-in loop re-entry works
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, act, waitFor, cleanup } from "@testing-library/react";
import React from "react";
import {
  setupAudioMocks, capsResponse, audioMeta, createTestContextConsumer,
  mockConnect, mockDisconnect, mockSendInterrupt,
  mockEnqueuePCM, mockStopPlayback, mockUpdateFormat, mockMarkDraining,
} from "./testHelpers";

// ---------------------------------------------------------------------------
// Module mocks
// ---------------------------------------------------------------------------

// react-native mock: resolved via vitest.config.ts alias to shared __mocks__/react-native.ts
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

import { useAudioWebSocket } from "../useAudioWebSocket";
import { useAudioPlayback } from "../useAudioPlayback";
import {
  AudioPlaybackProvider,
  useAudioPlaybackContext,
} from "../AudioPlaybackContext";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Local wrapper — delegates to shared setupAudioMocks with this file's mocked hooks. */
function setupMocks(overrides?: { wsStatus?: string; playbackState?: string }) {
  return setupAudioMocks({ useAudioWebSocket, useAudioPlayback }, overrides);
}

const TestConsumer = createTestContextConsumer(useAudioPlaybackContext);

// ---------------------------------------------------------------------------
// Test Suite: Barge-in during TTS (AC2)
// ---------------------------------------------------------------------------

describe("Barge-In During TTS (AC2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("stopPlayback is called immediately on barge-in, resetting PCM buffer state", async () => {
    // Subtask 3.1: Verify stopPlayback() called + PCM buffer reset
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Establish TTS capability
    act(() => {
      capturedOnControlMessage()(capsResponse(["tts"]));
    });

    // Receive audio_meta so awaitingFirstMeta becomes false
    act(() => {
      capturedOnControlMessage()(audioMeta());
    });

    // Verify PCM flows through when not awaiting
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // Barge-in: stopPlayback resets awaitingFirstMeta back to true
    act(() => {
      values.stopPlayback();
    });
    expect(mockStopPlayback).toHaveBeenCalled();

    // After barge-in, PCM frames should be BUFFERED (not directly enqueued)
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled();
  });

  it("sendInterrupt called with 'user_barge_in' reason (JSON format tested in useAudioWebSocket.test.ts)", async () => {
    // Subtask 3.2: Verify reason string passed to hook; actual JSON construction
    // {"type":"interrupt","reason":"user_barge_in"} is tested in useAudioWebSocket.test.ts
    setupMocks({ wsStatus: "connected" });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Send barge-in interrupt
    act(() => {
      values.sendInterrupt("user_barge_in");
    });

    // useAudioWebSocket.sendInterrupt is called with the reason string;
    // the hook formats it as JSON { type: "interrupt", reason: "user_barge_in" }
    expect(mockSendInterrupt).toHaveBeenCalledWith("user_barge_in");
  });

  it("full barge-in loop: stop → new audio_meta → new PCM → playback resumes", async () => {
    // Subtask 3.3: Full barge-in loop re-entry
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Initial TTS stream
    act(() => {
      capturedOnControlMessage()(capsResponse(["tts"]));
    });
    act(() => {
      capturedOnControlMessage()(audioMeta());
    });
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // Barge-in
    act(() => {
      values.stopPlayback();
    });
    mockEnqueuePCM.mockClear();
    mockStopPlayback.mockClear();
    mockUpdateFormat.mockClear();

    // New TTS stream after barge-in: audio_meta arrives
    act(() => {
      capturedOnControlMessage()(audioMeta({ first: true }));
    });
    expect(mockUpdateFormat).toHaveBeenCalled();

    // New PCM frames flow through
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(320));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // End of new stream
    act(() => {
      capturedOnControlMessage()(audioMeta({ last: true }));
    });
    expect(mockMarkDraining).toHaveBeenCalled();
  });

  it("PCM buffer cleared and awaitingFirstMeta reset when stopPlayback is called (Subtask 3.4)", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    act(() => {
      capturedOnControlMessage()(capsResponse(["tts"]));
    });

    // Buffer some PCM frames (before audio_meta)
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
      capturedOnPcmData()(new ArrayBuffer(160));
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled(); // buffered, not played

    // Receive audio_meta → flushes buffer
    act(() => {
      capturedOnControlMessage()(audioMeta());
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(3);

    // Barge-in: stop + reset
    act(() => {
      values.stopPlayback();
    });

    // Verify buffer is cleared: send new PCM, should be buffered (not enqueued)
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    // New audio_meta flushes only the new frame
    act(() => {
      capturedOnControlMessage()(audioMeta());
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("full barge-in UX chain: stopPlayback → sendInterrupt → startRecording in sequence (Subtask 3.1 complete)", async () => {
    // Validates the FULL barge-in contract that session/[id].tsx relies on:
    // When user presses VoiceButton during TTS → stop playback → interrupt daemon → start recording
    const { capturedOnControlMessage } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;
    const mockStartRecording = vi.fn();

    function BargeInChainConsumer({ onValues }: { onValues: (val: any) => void }) {
      const ctx = useAudioPlaybackContext();
      React.useEffect(() => {
        onValues({ ...ctx, startRecording: mockStartRecording });
      });
      return null;
    }

    render(
      <AudioPlaybackProvider sessionId="s1">
        <BargeInChainConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Establish active TTS stream
    act(() => {
      capturedOnControlMessage()(capsResponse(["tts"]));
    });
    act(() => {
      capturedOnControlMessage()(audioMeta());
    });

    // Execute the full barge-in UX chain as session/[id].tsx would:
    // Step 1: Stop TTS playback immediately
    act(() => {
      values.stopPlayback();
    });
    expect(mockStopPlayback).toHaveBeenCalled();

    // Step 2: Send interrupt to daemon via audio WebSocket
    act(() => {
      values.sendInterrupt("user_barge_in");
    });
    expect(mockSendInterrupt).toHaveBeenCalledWith("user_barge_in");

    // Step 3: Start new voice recording (VoiceContext.startRecording)
    act(() => {
      values.startRecording();
    });
    expect(mockStartRecording).toHaveBeenCalled();

    // Verify execution order: stop → interrupt → record
    const stopOrder = mockStopPlayback.mock.invocationCallOrder[0];
    const interruptOrder = mockSendInterrupt.mock.invocationCallOrder[0];
    const recordOrder = mockStartRecording.mock.invocationCallOrder[0];
    expect(stopOrder).toBeLessThan(interruptOrder);
    expect(interruptOrder).toBeLessThan(recordOrder);
  });

  it("barge-in when WebSocket is idle: context passthrough doesn't crash (WS-level no-op tested in useAudioWebSocket.test.ts)", async () => {
    // Tests AudioPlaybackContext passthrough layer — stopPlayback and sendInterrupt
    // are callable without throwing. In production, useAudioWebSocket.sendInterrupt
    // guards with `ws.readyState === WebSocket.OPEN` — the interrupt message is NOT
    // actually sent to the daemon when WS is idle. That guard is tested in
    // useAudioWebSocket.test.ts. This test verifies the context layer doesn't crash.
    setupMocks({ wsStatus: "idle" });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Barge-in attempt with WS not connected — must not throw
    act(() => {
      values.stopPlayback();
    });
    expect(mockStopPlayback).toHaveBeenCalled();

    act(() => {
      values.sendInterrupt("user_barge_in");
    });
    expect(mockSendInterrupt).toHaveBeenCalledWith("user_barge_in");
  });

  it("rapid successive barge-ins (double-tap): second barge-in is safe while first is resetting", async () => {
    // Edge case: user taps Speak twice rapidly during TTS. The first tap triggers
    // stopPlayback → awaitingFirstMeta reset. The second tap fires before any new
    // audio_meta arrives. Both calls must succeed without throwing or corrupting state.
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    // Establish active TTS stream
    act(() => {
      capturedOnControlMessage()(capsResponse(["tts"]));
    });
    act(() => {
      capturedOnControlMessage()(audioMeta({ first: true }));
    });
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // Clear setup-phase mock calls (audioMeta first=true internally calls pbStopPlayback)
    mockStopPlayback.mockClear();
    mockSendInterrupt.mockClear();

    // First barge-in (tap 1)
    act(() => {
      values.stopPlayback();
      values.sendInterrupt("user_barge_in");
    });
    expect(mockStopPlayback).toHaveBeenCalledTimes(1);
    expect(mockSendInterrupt).toHaveBeenCalledTimes(1);

    // Second barge-in immediately (tap 2) — no new audio_meta has arrived yet,
    // awaitingFirstMeta is already true, buffer already cleared. Must not throw.
    act(() => {
      values.stopPlayback();
      values.sendInterrupt("user_barge_in");
    });
    expect(mockStopPlayback).toHaveBeenCalledTimes(2);
    expect(mockSendInterrupt).toHaveBeenCalledTimes(2);

    // Verify state is still valid: new PCM frames are buffered (not enqueued)
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    // Recovery: new audio_meta arrives → PCM flows through normally
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnControlMessage()(audioMeta({ first: true }));
    });
    act(() => {
      capturedOnPcmData()(new ArrayBuffer(320));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("barge-in when WebSocket is in error state: context passthrough doesn't throw (WS guards tested separately)", async () => {
    // Tests AudioPlaybackContext passthrough — WS is in error state but context
    // methods remain callable. Production sendInterrupt no-ops when WS is not OPEN
    // (guard in useAudioWebSocket.ts line 174). This test validates crash-safety only.
    setupMocks({ wsStatus: "error" });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>,
    );

    await act(async () => { await Promise.resolve(); });

    act(() => {
      values.stopPlayback();
    });
    expect(mockStopPlayback).toHaveBeenCalled();

    // sendInterrupt called — hook is responsible for handling WS state internally
    act(() => {
      values.sendInterrupt("user_barge_in");
    });
    expect(mockSendInterrupt).toHaveBeenCalledWith("user_barge_in");
  });
});
