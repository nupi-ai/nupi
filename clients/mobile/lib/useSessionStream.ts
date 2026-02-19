import { useCallback, useEffect, useRef, useState } from "react";

import { getConnectionConfig, getToken } from "./storage";

export type StreamStatus = "connecting" | "connected" | "closed" | "error";

export interface WsEvent {
  type: "event";
  event_type: string;
  session_id: string;
  data?: Record<string, string>;
}

interface UseSessionStreamOptions {
  onOutput?: (data: ArrayBuffer) => void;
  onEvent?: (event: WsEvent) => void;
}

interface UseSessionStreamResult {
  status: StreamStatus;
  error: string | null;
  send: (data: string | ArrayBuffer) => void;
  close: () => void;
}

/**
 * React hook that manages a WebSocket connection to nupid's session
 * streaming bridge at /ws/session/{sessionId}.
 *
 * Binary frames from the server carry PTY output; text frames carry
 * JSON lifecycle events (resize_instruction, stopped, etc.).
 */
export function useSessionStream(
  sessionId: string,
  options: UseSessionStreamOptions = {}
): UseSessionStreamResult {
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const [error, setError] = useState<string | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const mountedRef = useRef(true);
  const onOutputRef = useRef(options.onOutput);
  const onEventRef = useRef(options.onEvent);

  // Keep callback refs up to date without re-triggering the effect.
  onOutputRef.current = options.onOutput;
  onEventRef.current = options.onEvent;

  const send = useCallback((data: string | ArrayBuffer) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(data);
    }
  }, []);

  const close = useCallback(() => {
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
    if (mountedRef.current) {
      setStatus("closed");
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    mountedRef.current = true;
    let ws: WebSocket | null = null;

    (async () => {
      try {
        const token = await getToken();
        const config = await getConnectionConfig();

        if (cancelled || !mountedRef.current) return;

        if (!token || !config) {
          setStatus("error");
          setError("Not authenticated — please pair first.");
          return;
        }

        const scheme = config.tls ? "wss" : "ws";
        const url =
          `${scheme}://${config.host}:${config.port}` +
          `/ws/session/${sessionId}?token=${encodeURIComponent(token)}&history=true`;

        ws = new WebSocket(url);
        ws.binaryType = "arraybuffer";
        wsRef.current = ws;

        ws.onopen = () => {
          if (cancelled || !mountedRef.current) return;
          setStatus("connected");
          setError(null);
        };

        ws.onmessage = (event: MessageEvent) => {
          if (cancelled || !mountedRef.current) return;

          if (event.data instanceof ArrayBuffer) {
            // Binary frame — PTY output.
            onOutputRef.current?.(event.data);
          } else if (typeof event.data === "string") {
            // Text frame — JSON event.
            try {
              const parsed = JSON.parse(event.data) as WsEvent;
              onEventRef.current?.(parsed);

              // Terminal session ended — mark stream closed.
              if (
                parsed.type === "event" &&
                parsed.event_type === "stopped"
              ) {
                setStatus("closed");
              }
            } catch {
              // Ignore malformed JSON frames.
            }
          }
        };

        ws.onerror = () => {
          if (cancelled || !mountedRef.current) return;
          setStatus("error");
          setError("WebSocket connection error");
        };

        ws.onclose = (event: CloseEvent) => {
          if (cancelled || !mountedRef.current) {
            wsRef.current = null;
            return;
          }
          // Use functional update to avoid stale closure — don't
          // overwrite "error" or already-"closed" state.
          setStatus((prev) => {
            if (prev === "error" || prev === "closed") return prev;
            return event.code === 1000 ? "closed" : "error";
          });
          if (event.code !== 1000) {
            setError((prev) => prev ?? `Connection closed (code ${event.code})`);
          }
          wsRef.current = null;
        };
      } catch (err) {
        if (!cancelled && mountedRef.current) {
          setStatus("error");
          setError(err instanceof Error ? err.message : "Connection failed");
        }
      }
    })();

    return () => {
      cancelled = true;
      mountedRef.current = false;
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
    };
    // sessionId is the only external dependency — reconnect on ID change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId]);

  return { status, error, send, close };
}
