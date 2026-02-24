import { useCallback, useEffect, useRef } from "react";

import { type AudioControlMessage, parseAudioMessage } from "./audioProtocol";
import { getConnectionConfig, getToken } from "./storage";
import { useSafeState } from "./useSafeState";
import { useTimeout } from "./useTimeout";

export type { AudioControlMessage, AudioFormat } from "./audioProtocol";
export { parseAudioMessage } from "./audioProtocol";

export type AudioWsStatus = "idle" | "connecting" | "connected" | "reconnecting" | "closed" | "error";

const MAX_WS_RECONNECT_ATTEMPTS = 3;

interface UseAudioWebSocketOptions {
  /** Called when a binary PCM frame arrives. */
  onPcmData?: (data: ArrayBuffer) => void;
  /** Called when a parsed text control message arrives. */
  onControlMessage?: (msg: AudioControlMessage) => void;
}

interface UseAudioWebSocketResult {
  status: AudioWsStatus;
  error: string | null;
  /** Send an interrupt message (e.g., barge-in). */
  sendInterrupt: (reason: string) => void;
  /** Gracefully disconnect from audio WebSocket. */
  disconnect: () => void;
  /** Manually trigger connection (used when TTS becomes available). */
  connect: () => void;
}

/**
 * React hook managing a WebSocket connection to nupid's audio streaming
 * bridge at /ws/audio/{sessionId}.
 *
 * Binary frames carry PCM audio data (TTS egress).
 * Text frames carry JSON control messages (audio_meta, capabilities, close_stream).
 *
 * Does NOT auto-connect on mount — call connect() when TTS playback is needed.
 */
export function useAudioWebSocket(
  sessionId: string,
  options: UseAudioWebSocketOptions = {}
): UseAudioWebSocketResult {
  const [status, setStatus, mountedRef] = useSafeState<AudioWsStatus>("idle");
  const [error, setError] = useSafeState<string | null>(null, mountedRef);
  const wsRef = useRef<WebSocket | null>(null);
  const cancelledRef = useRef(false);
  const onPcmDataRef = useRef(options.onPcmData);
  const onControlMessageRef = useRef(options.onControlMessage);
  const reconnectAttemptsRef = useRef(0);
  const reconnectTimer = useTimeout();
  const manualCloseRef = useRef(false);
  const isOpeningWsRef = useRef(false);

  // Keep callback refs up to date.
  onPcmDataRef.current = options.onPcmData;
  onControlMessageRef.current = options.onControlMessage;

  const openWebSocket = useCallback(async () => {
    if (cancelledRef.current || !mountedRef.current || isOpeningWsRef.current) return;
    isOpeningWsRef.current = true;

    try {
      const token = await getToken();
      const config = await getConnectionConfig();

      if (cancelledRef.current || !mountedRef.current) {
        isOpeningWsRef.current = false;
        return;
      }

      if (!token || !config) {
        isOpeningWsRef.current = false;
        setStatus("error");
        setError("Not authenticated — please pair first.");
        return;
      }

      const scheme = config.tls ? "wss" : "ws";
      const url =
        `${scheme}://${config.host}:${config.port}` +
        `/ws/audio/${sessionId}?token=${encodeURIComponent(token)}`;

      const ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;

      // Connection timeout — if WS doesn't open within 5s, close and let
      // onclose handle reconnection logic.
      const connectTimeout = setTimeout(() => {
        if (ws.readyState !== WebSocket.OPEN) {
          ws.close();
        }
      }, 5000);

      ws.onopen = () => {
        clearTimeout(connectTimeout);
        isOpeningWsRef.current = false;
        if (cancelledRef.current || !mountedRef.current) return;
        reconnectAttemptsRef.current = 0;
        setStatus("connected");
        setError(null);
        // Query daemon capabilities on connect.
        try {
          ws.send(JSON.stringify({ type: "capabilities" }));
        } catch {
          // Ignore — connection may close immediately.
        }
      };

      ws.onmessage = (event: MessageEvent) => {
        if (cancelledRef.current || !mountedRef.current) return;

        if (event.data instanceof ArrayBuffer) {
          onPcmDataRef.current?.(event.data);
        } else if (typeof event.data === "string") {
          // parseAudioMessage handles invalid JSON internally (returns "unknown" type).
          const parsed = parseAudioMessage(event.data);
          onControlMessageRef.current?.(parsed);
        }
      };

      ws.onerror = () => {
        // Let onclose handle reconnect.
      };

      ws.onclose = (event: CloseEvent) => {
        clearTimeout(connectTimeout);
        isOpeningWsRef.current = false;
        if (cancelledRef.current || !mountedRef.current) {
          wsRef.current = null;
          return;
        }

        wsRef.current = null;

        if (manualCloseRef.current) {
          setStatus("closed");
          return;
        }

        // Normal close
        if (event.code === 1000) {
          setStatus("closed");
          return;
        }

        // Unexpected close — try auto-reconnect
        if (reconnectAttemptsRef.current < MAX_WS_RECONNECT_ATTEMPTS) {
          reconnectAttemptsRef.current += 1;
          setStatus("reconnecting");
          const baseDelay = 1000 * Math.pow(2, reconnectAttemptsRef.current - 1);
          // Add jitter (50–100% of base delay) to avoid synchronized reconnections.
          const delay = baseDelay * (0.5 + Math.random() * 0.5);
          reconnectTimer.schedule(() => openWebSocket(), delay);
        } else {
          setStatus("error");
          setError("Audio connection lost");
        }
      };
    } catch {
      isOpeningWsRef.current = false;
      if (!cancelledRef.current) {
        setStatus("error");
        setError("Audio connection failed");
      }
    }
  }, [reconnectTimer, sessionId]);

  const sendInterrupt = useCallback((reason: string) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      try {
        ws.send(JSON.stringify({ type: "interrupt", reason }));
      } catch {
        // Ignore — connection may be closing.
      }
    }
  }, []);

  const disconnect = useCallback(() => {
    manualCloseRef.current = true;
    reconnectTimer.clear();
    const ws = wsRef.current;
    if (ws) {
      if (ws.readyState === WebSocket.OPEN) {
        try {
          ws.send(JSON.stringify({ type: "detach" }));
        } catch {
          // Ignore.
        }
      }
      ws.close();
      wsRef.current = null;
    }
    setStatus("closed");
  }, [reconnectTimer]);

  const connect = useCallback(() => {
    if (wsRef.current || isOpeningWsRef.current) return;
    manualCloseRef.current = false;
    cancelledRef.current = false;
    reconnectAttemptsRef.current = 0;
    setStatus("connecting");
    setError(null);
    openWebSocket();
  }, [openWebSocket]);

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      cancelledRef.current = true;
      reconnectTimer.clear();
      const ws = wsRef.current;
      if (ws) {
        ws.onopen = null;
        ws.onmessage = null;
        ws.onerror = null;
        ws.onclose = null;
        if (ws.readyState === WebSocket.OPEN) {
          try {
            ws.send(JSON.stringify({ type: "detach" }));
          } catch {
            // Ignore.
          }
        }
        ws.close();
        wsRef.current = null;
      }
    };
  }, [reconnectTimer]);

  return { status, error, sendInterrupt, disconnect, connect };
}
