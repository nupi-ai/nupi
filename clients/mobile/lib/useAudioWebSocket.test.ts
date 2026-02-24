// @vitest-environment jsdom
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";

class MockWebSocket {
  static OPEN = 1;
  static CONNECTING = 0;
  static CLOSING = 2;
  static CLOSED = 3;

  readyState = MockWebSocket.CONNECTING;
  binaryType = "blob";
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  url: string;
  sent: unknown[] = [];

  constructor(url: string) {
    this.url = url;
    instances.push(this);
  }
  send(data: unknown) {
    this.sent.push(data);
  }
  close() {
    this.readyState = MockWebSocket.CLOSED;
  }

  simulateOpen() {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.(new Event("open"));
  }
  simulateMessage(data: string | ArrayBuffer) {
    this.onmessage?.(new MessageEvent("message", { data }));
  }
  simulateClose(code = 1000) {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.(new CloseEvent("close", { code }));
  }
  simulateError() {
    this.onerror?.(new Event("error"));
  }
}

let instances: MockWebSocket[] = [];

vi.stubGlobal("WebSocket", MockWebSocket);

vi.mock("./storage", () => ({
  getToken: vi.fn(async () => "test-token"),
  getConnectionConfig: vi.fn(async () => ({
    host: "localhost",
    port: 8080,
    tls: false,
  })),
}));

import { getToken } from "./storage";
import { useAudioWebSocket } from "./useAudioWebSocket";

/**
 * Flush microtasks for openWebSocket's async operations:
 * await getToken(), await getConnectionConfig(), plus React batching.
 */
async function flushAsync() {
  for (let i = 0; i < 10; i++) {
    await Promise.resolve();
  }
}

describe("useAudioWebSocket", () => {
  const defaultOpts = {
    onPcmData: vi.fn(),
    onControlMessage: vi.fn(),
  };

  beforeEach(() => {
    vi.clearAllMocks();
    instances = [];
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("returns idle status initially", () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    expect(result.current.status).toBe("idle");
    expect(result.current.error).toBeNull();
  });

  it("transitions to connecting then connected on connect()", async () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    // After connect() + async resolution, WS should be created
    expect(instances.length).toBe(1);
    expect(instances[0].url).toContain("ws://localhost:8080/ws/audio/session-1");
    expect(instances[0].url).toContain("token=test-token");

    await act(async () => {
      instances[0].simulateOpen();
    });

    expect(result.current.status).toBe("connected");
  });

  it("connect() is a no-op if already connected", async () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    await act(async () => {
      instances[0].simulateOpen();
    });

    expect(instances.length).toBe(1);

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    // Should not create a second WebSocket
    expect(instances.length).toBe(1);
  });

  it("triggers onPcmData for binary messages", async () => {
    const onPcmData = vi.fn();
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", { ...defaultOpts, onPcmData })
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    await act(async () => {
      instances[0].simulateOpen();
    });

    const buffer = new ArrayBuffer(160);
    act(() => {
      instances[0].simulateMessage(buffer);
    });

    expect(onPcmData).toHaveBeenCalledWith(buffer);
  });

  it("triggers onControlMessage for text messages", async () => {
    const onControlMessage = vi.fn();
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", {
        ...defaultOpts,
        onControlMessage,
      })
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    await act(async () => {
      instances[0].simulateOpen();
    });

    const msg = JSON.stringify({
      type: "audio_meta",
      format: { encoding: "pcm_s16le", sample_rate: 24000, channels: 1, bit_depth: 16 },
      sequence: 1,
      first: true,
      last: false,
      duration_ms: 20,
    });
    act(() => {
      instances[0].simulateMessage(msg);
    });

    expect(onControlMessage).toHaveBeenCalledWith(
      expect.objectContaining({ type: "audio_meta" })
    );
  });

  it("sendInterrupt sends a JSON text frame with reason", async () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    await act(async () => {
      instances[0].simulateOpen();
    });

    act(() => {
      result.current.sendInterrupt("user_barge_in");
    });

    const sent = instances[0].sent;
    const interruptMsg = sent.find((s) => {
      try {
        return JSON.parse(s as string).type === "interrupt";
      } catch {
        return false;
      }
    });
    expect(interruptMsg).toBeDefined();
    const parsed = JSON.parse(interruptMsg as string);
    expect(parsed.type).toBe("interrupt");
    expect(parsed.reason).toBe("user_barge_in");
  });

  it("disconnect() sends detach, closes WS, and transitions to closed", async () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    await act(async () => {
      instances[0].simulateOpen();
    });

    await act(async () => {
      result.current.disconnect();
    });

    const sent = instances[0].sent;
    const hasDetach = sent.some((s) => {
      try {
        return JSON.parse(s as string).type === "detach";
      } catch {
        return false;
      }
    });
    expect(hasDetach).toBe(true);
    expect(instances[0].readyState).toBe(MockWebSocket.CLOSED);
    expect(result.current.status).toBe("closed");
  });

  it("schedules reconnection on unexpected close", async () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    await act(async () => {
      instances[0].simulateOpen();
    });

    await act(async () => {
      instances[0].simulateClose(1006); // abnormal close
    });

    expect(result.current.status).toBe("reconnecting");

    // Advance timer for first reconnect attempt (1s delay)
    await act(async () => {
      vi.advanceTimersByTime(1000);
      await flushAsync();
    });

    // Should have created a second WS instance
    expect(instances.length).toBeGreaterThan(1);
  });

  it("transitions to error when token is null (not authenticated)", async () => {
    vi.mocked(getToken).mockResolvedValueOnce(null);
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    expect(result.current.status).toBe("error");
    expect(result.current.error).toBe("Not authenticated — please pair first.");
    // No WebSocket should be created.
    expect(instances.length).toBe(0);
  });

  it("closes WebSocket on 5s connection timeout", async () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    expect(instances.length).toBe(1);
    const ws = instances[0];
    // WS is still in CONNECTING state — onopen never fires.
    expect(ws.readyState).toBe(MockWebSocket.CONNECTING);

    // Advance past the 5s connection timeout.
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });

    // Timeout should have called ws.close().
    expect(ws.readyState).toBe(MockWebSocket.CLOSED);
  });

  it("transitions to error after all reconnect attempts exhausted", async () => {
    const { result } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    // Initial WS opens then closes abnormally — triggers first reconnect.
    // reconnectAttemptsRef resets to 0 on open, then 0→1 on close.
    await act(async () => {
      instances[0].simulateOpen();
    });
    await act(async () => {
      instances[0].simulateClose(1006);
    });
    expect(result.current.status).toBe("reconnecting");

    // Exhaust all 3 reconnect attempts. Each reconnected WS
    // fails without ever opening (simulated abnormal close).
    for (let i = 0; i < 3; i++) {
      // Advance timer past jittered backoff delay.
      await act(async () => {
        vi.advanceTimersByTime(10_000);
        await flushAsync();
      });

      const wsIdx = instances.length - 1;
      await act(async () => {
        instances[wsIdx].simulateClose(1006);
      });
    }

    expect(result.current.status).toBe("error");
    expect(result.current.error).toBe("Audio connection lost");
  });

  it("cleans up WebSocket on unmount", async () => {
    const { result, unmount } = renderHook(() =>
      useAudioWebSocket("session-1", defaultOpts)
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    await act(async () => {
      instances[0].simulateOpen();
    });

    const ws = instances[0];
    expect(ws.readyState).toBe(MockWebSocket.OPEN);

    unmount();

    expect(ws.readyState).toBe(MockWebSocket.CLOSED);
  });
});
