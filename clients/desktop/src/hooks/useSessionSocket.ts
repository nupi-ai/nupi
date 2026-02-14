import { useState, useEffect, useRef, useCallback } from 'react';
import { parseBinaryFrame, BINARY_FRAME_OUTPUT } from '../utils/protocol';
import type { Session } from '../types/session';

export type { Session };

export interface SessionSocketCallbacks {
  onBinaryOutput: (sessionId: string, payload: Uint8Array) => void;
  onResizeInstructions: (sessionId: string, instructions: any[]) => void;
  onSessionStopped: (sessionId: string) => void;
}

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

    // Don't create new connection if we already have one
    if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      return;
    }

    // Connect WebSocket for session management
    const ws = new WebSocket(`ws://127.0.0.1:${daemonPort}/ws`);

    ws.onopen = () => {
      // Request sessions list on connect
      ws.send(JSON.stringify({
        type: 'list',
      }));
    };

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
      let msg: any;
      try {
        msg = JSON.parse(raw);
      } catch (error) {
        console.error('[WS] Failed to parse message', error, raw);
        return;
      }

      if (msg.type === 'attached' && msg.sessionId === attachedSessionId.current) {
        return;
      }

      if (msg.type === 'sessions_list') {
        if (msg.data && Array.isArray(msg.data)) {
          const sortedSessions = [...msg.data].sort((a: Session, b: Session) =>
            new Date(a.start_time).getTime() - new Date(b.start_time).getTime()
          );
          setSessions(sortedSessions);
          if (sortedSessions.length > 0 && !activeSessionIdRef.current) {
            setActiveSessionIdRef.current(sortedSessions[0].id);
          }
        }
        return;
      }

      if (msg.type === 'session_created') {
        if (msg.data) {
          setSessions(prev => {
            const exists = prev.some(s => s.id === msg.data.id);
            if (exists) {
              return prev;
            }

            const newSessions = [...prev, msg.data];
            const sorted = newSessions.sort((a: Session, b: Session) =>
              new Date(a.start_time).getTime() - new Date(b.start_time).getTime()
            );

            if (!activeSessionIdRef.current && sorted.length === 1) {
              setActiveSessionIdRef.current(sorted[0].id);
            }

            return sorted;
          });
        }
        return;
      }

      if (msg.type === 'session_killed') {
        // Don't remove session - just wait for session_status_changed to 'stopped'
        // Sessions should remain visible after being killed
        console.log('[WS] Session killed:', msg.sessionId);
        return;
      }

      if (msg.type === 'session_status_changed') {
        if (msg.sessionId && msg.data) {
          // Update sessions state (for UI rendering only)
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, status: msg.data } : s
          ));

          // Update terminal settings directly (no re-render)
          if (msg.sessionId === attachedSessionId.current && msg.data === 'stopped') {
            callbacksRef.current.onSessionStopped(msg.sessionId);
          }
        }
        return;
      }

      if (msg.type === 'session_mode_changed') {
        if (msg.sessionId && msg.data && typeof msg.data.mode === 'string') {
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, mode: msg.data.mode } : s
          ));
        }
        return;
      }

      if (msg.type === 'tool_detected') {
        if (msg.sessionId && msg.data) {
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, ...msg.data } : s
          ));
        }
        return;
      }

      if (msg.type === 'resize_instruction') {
        if (msg.sessionId && msg.sessionId === attachedSessionId.current) {
          if (msg.data && Array.isArray(msg.data.instructions)) {
            callbacksRef.current.onResizeInstructions(msg.sessionId, msg.data.instructions);
          }
        }
        return;
      }
    };

    ws.onmessage = (event) => {
      if (typeof event.data === 'string') {
        handleTextMessage(event.data);
        return;
      }

      if (event.data instanceof Blob) {
        event.data.arrayBuffer()
          .then(handleBinaryMessage)
          .catch(err => console.error('[WS] Failed to process binary message', err));
        return;
      }

      if (event.data instanceof ArrayBuffer) {
        handleBinaryMessage(event.data);
        return;
      }

      console.warn('[WS] Received unsupported message payload', event.data);
    };

    ws.onerror = (error) => {
      console.error('WebSocket error:', error);
    };

    ws.onclose = () => {
      console.log('WebSocket disconnected');
    };

    wsRef.current = ws;

    return () => {
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
