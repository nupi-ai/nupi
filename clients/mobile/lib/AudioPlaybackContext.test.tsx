// @vitest-environment jsdom
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, act, waitFor } from "@testing-library/react";
import React from "react";

const mockConnect = vi.fn();
const mockDisconnect = vi.fn();
const mockSendInterrupt = vi.fn();
const mockEnqueuePCM = vi.fn();
const mockStopPlayback = vi.fn();
const mockUpdateFormat = vi.fn();
const mockMarkDraining = vi.fn();

vi.mock("./useAudioWebSocket", () => ({
  useAudioWebSocket: vi.fn(),
}));
vi.mock("./useAudioPlayback", () => ({
  useAudioPlayback: vi.fn(),
}));
vi.mock("./storage", () => ({
  getTtsEnabled: vi.fn(async () => true),
  addTtsChangeListener: vi.fn(() => () => {}),
}));

import { useAudioWebSocket } from "./useAudioWebSocket";
import { useAudioPlayback } from "./useAudioPlayback";
import {
  AudioPlaybackProvider,
  useAudioPlaybackContext,
} from "./AudioPlaybackContext";
import type { AudioControlMessage } from "./audioProtocol";

function setupMocks(overrides?: {
  wsStatus?: string;
  playbackState?: string;
}) {
  let capturedOnPcmData: ((data: ArrayBuffer) => void) | undefined;
  let capturedOnControlMessage: ((msg: AudioControlMessage) => void) | undefined;

  (useAudioWebSocket as any).mockImplementation(
    (_sid: string, opts: any) => {
      capturedOnPcmData = opts.onPcmData;
      capturedOnControlMessage = opts.onControlMessage;
      return {
        status: overrides?.wsStatus ?? "idle",
        error: null,
        sendInterrupt: mockSendInterrupt,
        disconnect: mockDisconnect,
        connect: mockConnect,
      };
    }
  );

  (useAudioPlayback as any).mockImplementation((opts?: any) => {
    return {
      playbackState: overrides?.playbackState ?? "idle",
      playbackError: null,
      enqueuePCM: mockEnqueuePCM,
      stopPlayback: mockStopPlayback,
      updateFormat: mockUpdateFormat,
      markDraining: mockMarkDraining,
    };
  });

  return {
    capturedOnPcmData: () => capturedOnPcmData,
    capturedOnControlMessage: () => capturedOnControlMessage,
  };
}

/** Helper to build a typed capabilities_response message. */
function capsResponse(streams: string[]): AudioControlMessage {
  return {
    type: "capabilities_response",
    capture: [],
    playback: streams.map((s) => ({ stream_id: s, ready: true })),
  };
}

/** Helper to build a typed audio_meta message. */
function audioMeta(overrides?: { last?: boolean; first?: boolean }): AudioControlMessage {
  return {
    type: "audio_meta",
    format: { encoding: "pcm_s16le", sample_rate: 24000, channels: 1, bit_depth: 16 },
    sequence: 1,
    first: overrides?.first ?? false,
    last: overrides?.last ?? false,
    duration_ms: 20,
  };
}

function TestConsumer({ onValues }: { onValues: (val: any) => void }) {
  const ctx = useAudioPlaybackContext();
  onValues(ctx);
  return null;
}

describe("AudioPlaybackContext", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("provides default context values", () => {
    setupMocks();
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    expect(values.playbackState).toBe("idle");
    expect(values.ttsEnabled).toBe(false);
  });

  it("enables TTS when capabilities_response has playback streams and user pref is true", async () => {
    const { capturedOnControlMessage } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      capturedOnControlMessage()?.(capsResponse(["tts"]));
    });

    await waitFor(() => {
      expect(values.ttsEnabled).toBe(true);
    });
  });

  it("disables TTS and disconnects WS when capabilities_response has no playback streams (AC5)", async () => {
    const { capturedOnControlMessage } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      capturedOnControlMessage()?.(capsResponse([]));
    });

    await waitFor(() => {
      expect(values.ttsEnabled).toBe(false);
    });

    expect(mockDisconnect).toHaveBeenCalled();
  });

  it("resets PCM buffering state on barge-in (stopPlayback)", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      capturedOnControlMessage()?.(capsResponse(["tts"]));
    });

    // Send audio_meta so awaitingFirstMeta becomes false, then send PCM.
    act(() => {
      capturedOnControlMessage()?.(audioMeta());
    });

    mockEnqueuePCM.mockClear();

    // Verify PCM flows directly when not awaiting.
    act(() => {
      capturedOnPcmData()?.(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);

    // Barge-in: stopPlayback resets awaitingFirstMeta back to true.
    act(() => {
      values.stopPlayback();
    });
    expect(mockStopPlayback).toHaveBeenCalled();

    // After barge-in, PCM frames should be BUFFERED (not directly enqueued)
    // because awaitingFirstMeta was reset to true.
    mockEnqueuePCM.mockClear();
    act(() => {
      capturedOnPcmData()?.(new ArrayBuffer(160));
    });
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    // audio_meta arrival flushes the buffered frame.
    act(() => {
      capturedOnControlMessage()?.(audioMeta());
    });
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("buffers PCM frames until audio_meta arrives then flushes", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      capturedOnControlMessage()?.(capsResponse(["tts"]));
    });

    const buf1 = new ArrayBuffer(160);
    const buf2 = new ArrayBuffer(160);

    act(() => {
      capturedOnPcmData()?.(buf1);
      capturedOnPcmData()?.(buf2);
    });

    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    act(() => {
      capturedOnControlMessage()?.(audioMeta());
    });

    expect(mockUpdateFormat).toHaveBeenCalledWith(
      expect.objectContaining({ sample_rate: 24000 })
    );
    expect(mockEnqueuePCM).toHaveBeenCalledTimes(2);
  });

  it("drops buffers when hitting MAX_PENDING_PCM_FRAMES cap", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      capturedOnControlMessage()?.(capsResponse(["tts"]));
    });

    act(() => {
      for (let i = 0; i < 600; i++) {
        capturedOnPcmData()?.(new ArrayBuffer(160));
      }
    });

    act(() => {
      capturedOnControlMessage()?.(audioMeta());
    });

    expect(mockEnqueuePCM).toHaveBeenCalled();
    const callCount = mockEnqueuePCM.mock.calls.length;
    expect(callCount).toBeLessThanOrEqual(500);
  });

  it("close_stream handler resets awaitingFirstMeta state (M1 fix)", async () => {
    const { capturedOnControlMessage, capturedOnPcmData } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      capturedOnControlMessage()?.(capsResponse(["tts"]));
    });

    act(() => {
      capturedOnControlMessage()?.(audioMeta());
    });

    act(() => {
      capturedOnControlMessage()?.({ type: "close_stream" });
    });

    mockEnqueuePCM.mockClear();

    const newBuf = new ArrayBuffer(160);
    act(() => {
      capturedOnPcmData()?.(newBuf);
    });

    // After close_stream, awaitingFirstMeta is reset — PCM should buffer, not flush
    expect(mockEnqueuePCM).not.toHaveBeenCalled();

    act(() => {
      capturedOnControlMessage()?.(audioMeta());
    });

    expect(mockEnqueuePCM).toHaveBeenCalledTimes(1);
  });

  it("audio_meta.last triggers markDraining", async () => {
    const { capturedOnControlMessage } = setupMocks({
      wsStatus: "connected",
    });
    let values: any;

    render(
      <AudioPlaybackProvider sessionId="s1">
        <TestConsumer onValues={(v) => (values = v)} />
      </AudioPlaybackProvider>
    );

    await act(async () => {
      await Promise.resolve();
    });

    act(() => {
      capturedOnControlMessage()?.(capsResponse(["tts"]));
    });

    act(() => {
      capturedOnControlMessage()?.(audioMeta({ last: true }));
    });

    expect(mockMarkDraining).toHaveBeenCalled();
  });
});
