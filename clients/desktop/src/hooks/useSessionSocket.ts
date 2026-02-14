import { useState, useEffect, useRef, useCallback } from 'react';
import { parseBinaryFrame, BINARY_FRAME_OUTPUT } from '../utils/protocol';
import { parseServerMessage } from '../types/messages';
import type { Session } from '../types/session';

export type { Session };

export interface SessionSocketCallbacks {
  onBinaryOutput: (sessionId: string, payload: Uint8Array) => void;
  onResizeInstructions: (sessionId: string, instructions: any[]) => void;
  onSessionStopped: (sessionId: string) => void;
}

const BACKOFF_INITIAL = 500;
const BACKOFF_MAX = 10_000;

export function useSessionSocket(
  daemonPort: number | null,
  activeSessionId: string | null,
  setActiveSessionId: (id: string | null) => void,
  attachedSessionId: React.MutableRefObject<string | null>,
  callbacks: SessionSocketCallbacks,
): {
  sessions: Session[];
  sendMessage: (msg: object) => void;
} {
  const [sessions, setSessions] = useState<Session[]>([]);
  const wsRef = useRef<WebSocket | null>(null);

  // Store callbacks and state accessors in refs to avoid stale closures
  const callbacksRef = useRef(callbacks);
  callbacksRef.current = callbacks;
  const activeSessionIdRef = useRef(activeSessionId);
  activeSessionIdRef.current = activeSessionId;
  const setActiveSessionIdRef = useRef(setActiveSessionId);
  setActiveSessionIdRef.current = setActiveSessionId;

  // Connect to WebSocket when port is available (for session list)
  useEffect(() => {
    if (!daemonPort) return;

    let intentionalClose = false;
    let backoff = BACKOFF_INITIAL;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

    const handleBinaryMessage = (buffer: ArrayBuffer) => {
      const frame = parseBinaryFrame(buffer);
      if (!frame) return;

      switch (frame.frameType) {
        case BINARY_FRAME_OUTPUT: {
          if (frame.sessionId !== attachedSessionId.current) {
            return;
          }
          if (frame.payload.length === 0) {
            return;
          }
          callbacksRef.current.onBinaryOutput(frame.sessionId, frame.payload);
          break;
        }
        default:
          console.warn('[WS] Unknown binary frame type:', frame.frameType);
      }
    };

    const handleTextMessage = (raw: string) => {
      const msg = parseServerMessage(raw);
      if (!msg) return;

      switch (msg.type) {
        case 'attached':
          // Ignore attach confirmations (no action needed on client)
          break;

        case 'sessions_list': {
          if (!Array.isArray(msg.data)) break;
          const sortedSessions = [...msg.data].sort((a: Session, b: Session) =>
            new Date(a.start_time).getTime() - new Date(b.start_time).getTime()
          );
          setSessions(sortedSessions);
          if (sortedSessions.length > 0 && !activeSessionIdRef.current) {
            setActiveSessionIdRef.current(sortedSessions[0].id);
          }
          break;
        }

        case 'session_created': {
          if (!msg.data) break;
          setSessions(prev => {
            const exists = prev.some(s => s.id === msg.data.id);
            if (exists) return prev;

            const newSessions = [...prev, msg.data];
            const sorted = newSessions.sort((a: Session, b: Session) =>
              new Date(a.start_time).getTime() - new Date(b.start_time).getTime()
            );

            if (!activeSessionIdRef.current && sorted.length === 1) {
              setActiveSessionIdRef.current(sorted[0].id);
            }

            return sorted;
          });
          break;
        }

        case 'session_killed':
          console.log('[WS] Session killed:', msg.sessionId);
          break;

        case 'session_status_changed': {
          if (!msg.sessionId || !msg.data) break;
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, status: msg.data } : s
          ));

          if (msg.sessionId === attachedSessionId.current && msg.data === 'stopped') {
            callbacksRef.current.onSessionStopped(msg.sessionId);
          }
          break;
        }

        case 'session_mode_changed': {
          if (!msg.sessionId || !msg.data || typeof msg.data.mode !== 'string') break;
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, mode: msg.data.mode } : s
          ));
          break;
        }

        case 'tool_detected': {
          if (!msg.sessionId || !msg.data) break;
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, ...msg.data } : s
          ));
          break;
        }

        case 'resize_instruction': {
          if (msg.sessionId === attachedSessionId.current && msg.data && Array.isArray(msg.data.instructions)) {
            callbacksRef.current.onResizeInstructions(msg.sessionId, msg.data.instructions);
          }
          break;
        }

        case 'detached':
          console.log('[WS] Detached from session:', msg.sessionId);
          break;

        case 'error':
          console.error('[WS] Server error:', msg.data);
          break;
      }
    };

    const connect = () => {
      // Don't reconnect if cleanup has run
      if (intentionalClose) return;

      const ws = new WebSocket(`ws://127.0.0.1:${daemonPort}/ws`);

      ws.onopen = () => {
        backoff = BACKOFF_INITIAL;
        console.log('[WS] Connected');

        ws.send(JSON.stringify({ type: 'list' }));

        // Re-attach to the active session after reconnect
        if (attachedSessionId.current) {
          ws.send(JSON.stringify({
            type: 'attach',
            data: attachedSessionId.current,
          }));
        }
      };

      ws.onmessage = (event) => {
        if (typeof event.data === 'string') {
          handleTextMessage(event.data);
        } else if (event.data instanceof Blob) {
          event.data.arrayBuffer()
            .then(handleBinaryMessage)
            .catch(err => console.error('[WS] Failed to process binary message', err));
        } else if (event.data instanceof ArrayBuffer) {
          handleBinaryMessage(event.data);
        }
      };

      ws.onerror = (error) => {
        console.error('[WS] Error:', error);
      };

      ws.onclose = () => {
        if (wsRef.current === ws) {
          wsRef.current = null;
        }

        if (intentionalClose) {
          console.log('[WS] Disconnected (intentional)');
          return;
        }

        console.log(`[WS] Reconnecting in ${backoff}ms...`);
        reconnectTimer = setTimeout(() => {
          reconnectTimer = null;
          connect();
        }, backoff);
        backoff = Math.min(backoff * 2, BACKOFF_MAX);
      };

      wsRef.current = ws;
    };

    connect();

    return () => {
      intentionalClose = true;
      if (reconnectTimer !== null) {
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
      if (wsRef.current) {
        wsRef.current.close();
        wsRef.current = null;
      }
    };
  }, [daemonPort]); // Don't reconnect on activeSessionId change!

  const sendMessage = useCallback((msg: object) => {
    if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg));
    }
  }, []);

  return { sessions, sendMessage };
}
