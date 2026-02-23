import { useCallback, useEffect, useRef } from "react";

import { getConnectionConfig, getToken } from "./storage";
import {
  parseSessionStreamEvent,
  type SessionStreamEvent,
} from "./sessionStreamEvents";
import { useSafeState } from "./useSafeState";
import { useTimeout } from "./useTimeout";

export type StreamStatus = "connecting" | "connected" | "reconnecting" | "closed" | "error";

export type WsEvent = SessionStreamEvent;

interface UseSessionStreamOptions {
  onOutput?: (data: ArrayBuffer) => void;
  onEvent?: (event: WsEvent) => void;
  onReconnected?: () => void;
}

interface UseSessionStreamResult {
  status: StreamStatus;
  error: string | null;
  reconnectAttempts: number;
  send: (data: string | ArrayBuffer) => void;
  close: () => void;
  reconnect: () => void;
}

const MAX_WS_RECONNECT_ATTEMPTS = 3;

/**
 * React hook that manages a WebSocket connection to nupid's session
 * streaming bridge at /ws/session/{sessionId}.
 *
 * Binary frames from the server carry PTY output; text frames carry
 * JSON lifecycle events (resize_instruction, stopped, etc.).
 *
 * Includes auto-reconnect with exponential backoff on unexpected close.
 */
export function useSessionStream(
  sessionId: string,
  options: UseSessionStreamOptions = {}
): UseSessionStreamResult {
  const [status, setStatus, mountedRef] = useSafeState<StreamStatus>("connecting");
  const [error, setError] = useSafeState<string | null>(null, mountedRef);
  const [reconnectAttempts, setReconnectAttempts] = useSafeState(0, mountedRef);
  const wsRef = useRef<WebSocket | null>(null);
  const cancelledRef = useRef(false);
  const onOutputRef = useRef(options.onOutput);
  const onEventRef = useRef(options.onEvent);
  const sessionStoppedRef = useRef(false);
  const reconnectAttemptsRef = useRef(0);
  const reconnectTimer = useTimeout();
  const manualCloseRef = useRef(false);
  const isOpeningWsRef = useRef(false);
  const onReconnectedRef = useRef(options.onReconnected);
  const hasConnectedRef = useRef(false);

  // Keep callback refs up to date without re-triggering the effect.
  onOutputRef.current = options.onOutput;
  onEventRef.current = options.onEvent;
  onReconnectedRef.current = options.onReconnected;

  const send = useCallback((data: string | ArrayBuffer) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(data);
    }
  }, []);

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
        setError("Not authenticated \u2014 please pair first.");
        return;
      }

      const scheme = config.tls ? "wss" : "ws";
      const url =
        `${scheme}://${config.host}:${config.port}` +
        `/ws/session/${sessionId}?token=${encodeURIComponent(token)}&history=true`;

      const ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;

      ws.onopen = () => {
        isOpeningWsRef.current = false;
        if (cancelledRef.current || !mountedRef.current) return;
        // Signal reconnect so consumer can clear state before history replay
        if (hasConnectedRef.current) {
          onReconnectedRef.current?.();
        }
        hasConnectedRef.current = true;
        reconnectAttemptsRef.current = 0;
        setReconnectAttempts(0);
        setStatus("connected");
        setError(null);
      };

      ws.onmessage = (event: MessageEvent) => {
        if (cancelledRef.current || !mountedRef.current) return;

        if (event.data instanceof ArrayBuffer) {
          // Binary frame — PTY output.
          onOutputRef.current?.(event.data);
        } else if (typeof event.data === "string") {
          // Text frame — JSON event.
          try {
            const parsed = parseSessionStreamEvent(event.data);
            onEventRef.current?.(parsed);

            // Terminal session ended — mark stream closed, no reconnect.
            if (
              parsed.type === "event" &&
              parsed.event_type === "stopped"
            ) {
              sessionStoppedRef.current = true;
              setStatus("closed");
            }
          } catch {
            // Ignore malformed JSON frames.
          }
        }
      };

      ws.onerror = () => {
        // Let onclose handle the reconnect decision — avoid duplicate transitions.
      };

      ws.onclose = (event: CloseEvent) => {
        isOpeningWsRef.current = false;
        if (cancelledRef.current || !mountedRef.current) {
          wsRef.current = null;
          return;
        }

        wsRef.current = null;

        // Session ended normally — always "closed" regardless of close code
        if (sessionStoppedRef.current) {
          setStatus("closed");
          return;
        }

        // Manual close — derive status from close code
        if (manualCloseRef.current) {
          setStatus((prev) => {
            if (prev === "error" || prev === "closed") return prev;
            return event.code === 1000 ? "closed" : "error";
          });
          if (event.code !== 1000) {
            setError((prev) => prev ?? "Connection lost \u2014 please try again.");
          }
          return;
        }

        // Normal close (code 1000) — no reconnect needed
        if (event.code === 1000) {
          setStatus("closed");
          return;
        }

        // Unexpected close — try auto-reconnect
        if (reconnectAttemptsRef.current < MAX_WS_RECONNECT_ATTEMPTS) {
          reconnectAttemptsRef.current += 1;
          setReconnectAttempts(reconnectAttemptsRef.current);
          setStatus("reconnecting");
          const delay = 1000 * Math.pow(2, reconnectAttemptsRef.current - 1);
          reconnectTimer.schedule(() => openWebSocket(), delay);
        } else {
          setStatus("error");
          setError("Connection lost \u2014 tap to reconnect");
        }
      };
    } catch (err) {
      isOpeningWsRef.current = false;
      if (!cancelledRef.current) {
        setStatus("error");
        setError("Connection failed \u2014 tap to reconnect");
      }
    }
  }, [reconnectTimer, sessionId]);

  const close = useCallback(() => {
    manualCloseRef.current = true;
    // Cancel any pending reconnect
    reconnectTimer.clear();
    const ws = wsRef.current;
    if (ws) {
      // Send detach control message before closing.
      if (ws.readyState === WebSocket.OPEN) {
        try {
          ws.send(JSON.stringify({ type: "detach" }));
        } catch {
          // Ignore — connection may already be closing.
        }
      }
      ws.close();
      wsRef.current = null;
    }
    setStatus("closed");
  }, [reconnectTimer]);

  const reconnect = useCallback(() => {
    // Manual reconnect triggered by user — reset attempts and reopen
    if (sessionStoppedRef.current) return; // Session is done
    if (isOpeningWsRef.current) return; // Prevent double-tap race
    manualCloseRef.current = false;
    reconnectTimer.clear();
    const ws = wsRef.current;
    if (ws) {
      // Null handlers to prevent old onclose from interfering with new connection
      ws.onopen = null;
      ws.onmessage = null;
      ws.onerror = null;
      ws.onclose = null;
      ws.close();
      wsRef.current = null;
    }
    // Reset state for fresh connection
    reconnectAttemptsRef.current = 0;
    setReconnectAttempts(0);
    setStatus("connecting");
    setError(null);
    openWebSocket();
  }, [openWebSocket, reconnectTimer]);

  useEffect(() => {
    cancelledRef.current = false;
    mountedRef.current = true;
    sessionStoppedRef.current = false;
    manualCloseRef.current = false;
    reconnectAttemptsRef.current = 0;
    isOpeningWsRef.current = false;
    hasConnectedRef.current = false;

    openWebSocket();

    return () => {
      cancelledRef.current = true;
      mountedRef.current = false;
      reconnectTimer.clear();
      const ws = wsRef.current;
      if (ws) {
        // Null handlers BEFORE close to prevent stale onclose from firing
        // after the next effect run resets cancelledRef/mountedRef — same
        // pattern used in reconnect() (lines 223-226).
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
  }, [openWebSocket, reconnectTimer]);

  return { status, error, reconnectAttempts, send, close, reconnect };
}
