import { useState, useRef } from 'react';
import { useSessionSocket } from '../hooks/useSessionSocket';
import type { Session } from '../types/session';
import { useTerminalInstance } from '../hooks/useTerminalInstance';
import { SessionTabs } from './SessionTabs';
import { KillConfirmDialog } from './KillConfirmDialog';
import '@xterm/xterm/css/xterm.css';

interface SessionsProps {
  daemonPort: number | null;
  onPlayRecording?: (sessionId: string) => void;
}

export function Sessions({ daemonPort, onPlayRecording }: SessionsProps) {
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null);
  const [killConfirmDialog, setKillConfirmDialog] = useState<{sessionId: string, sessionName: string} | null>(null);

  // Ref indirection for sendMessage — resolves circular dependency between hooks.
  // Terminal hook needs sendMessage (from socket), socket hook needs attachedSessionId (from terminal).
  // The ref is populated after useSessionSocket returns, before effects fire.
  const sendMessageRef = useRef<(msg: object) => void>(() => {});

  // Shared ref for active session — updated after socket returns sessions each render.
  // Terminal hook reads this via ref (not as a dep) so only activeSessionId changes trigger re-init.
  const activeSessionRef = useRef<Session | undefined>(undefined);

  // Terminal hook — creates xterm instance, scrollbar, resize logic
  const terminal = useTerminalInstance(
    activeSessionId,
    activeSessionRef,
    (msg) => sendMessageRef.current(msg),
  );

  // Socket hook — WebSocket connection, session state, message dispatch
  const socket = useSessionSocket(
    daemonPort,
    activeSessionId,
    setActiveSessionId,
    terminal.attachedSessionId,
    {
      onBinaryOutput: (_sessionId, payload) => terminal.controls.writeChunk(payload),
      onResizeInstructions: (_sessionId, instructions) => terminal.controls.applyResizeInstructions(instructions),
      onSessionStopped: () => terminal.controls.markSessionStopped(),
    },
  );

  // Wire up refs — these assignments happen during render, before effects fire
  sendMessageRef.current = socket.sendMessage;
  activeSessionRef.current = socket.sessions.find(s => s.id === activeSessionId);

  // Kill session handlers
  const handleKillSessionClick = (sessionId: string, sessionName: string) => {
    console.log('[KILL] Button clicked!', sessionId, sessionName);
    setKillConfirmDialog({ sessionId, sessionName });
  };

  const confirmKillSession = () => {
    if (!killConfirmDialog) return;

    const { sessionId } = killConfirmDialog;
    console.log('[KILL] Confirmed, killing session:', sessionId);
    socket.sendMessage({ type: 'kill', data: sessionId });
    setKillConfirmDialog(null);
  };

  const cancelKillSession = () => {
    console.log('[KILL] Cancelled');
    setKillConfirmDialog(null);
  };

  // Don't show "no sessions" message immediately - keep WebSocket listening
  // This allows the UI to react when sessions are started

  return (
    <div style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0, overflow: 'hidden' }}>
      {/* Tabs at top */}
      <SessionTabs
        sessions={socket.sessions}
        activeSessionId={activeSessionId}
        onSelectSession={setActiveSessionId}
        onKillSession={handleKillSessionClick}
        onPlayRecording={onPlayRecording}
      />

      {/* Terminal */}
      <div
        style={{
          flex: 1,
          backgroundColor: '#262626', // Match terminal container background
          padding: '8px 8px 0 8px',
          boxSizing: 'border-box',
          minHeight: 0,
          display: 'flex',
          position: 'relative',
        }}
      >
        <div
          ref={terminal.refs.terminalRef}
          style={{
            flex: 1,
            minHeight: 0,
            overflowX: 'auto',
            overflowY: 'hidden',
            paddingRight: '24px',
          }}
        />
        <div
          ref={terminal.refs.customScrollbarRef}
          style={{
            position: 'absolute',
            top: '8px',
            right: '0',
            bottom: '8px',
            width: '16px',
            overflowY: 'auto',
            overflowX: 'hidden',
            visibility: 'hidden',
            pointerEvents: 'none',
            backgroundColor: 'rgba(38, 38, 38, 0.4)',
            borderRadius: '6px 0 0 6px',
            willChange: 'scroll-position',
            scrollBehavior: 'auto',
          }}
        >
          <div
            ref={terminal.refs.customScrollbarContentRef}
            style={{
              width: '100%',
              background: 'transparent',
            }}
          />
        </div>
      </div>

      {/* Kill confirmation dialog */}
      {killConfirmDialog && (
        <KillConfirmDialog
          sessionName={killConfirmDialog.sessionName}
          onConfirm={confirmKillSession}
          onCancel={cancelKillSession}
        />
      )}
    </div>
  );
}
