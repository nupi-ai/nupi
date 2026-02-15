import { useState } from 'react';
import { useSession } from '../hooks/useSession';
import { SessionTabs } from './SessionTabs';
import { KillConfirmDialog } from './KillConfirmDialog';
import '@xterm/xterm/css/xterm.css';

interface SessionsProps {
  daemonPort: number | null;
  onPlayRecording?: (sessionId: string) => void;
}

export function Sessions({ daemonPort, onPlayRecording }: SessionsProps) {
  const session = useSession(daemonPort);
  const [killConfirmDialog, setKillConfirmDialog] = useState<{sessionId: string, sessionName: string} | null>(null);

  // Kill session handlers
  const handleKillSessionClick = (sessionId: string, sessionName: string) => {
    console.log('[KILL] Button clicked!', sessionId, sessionName);
    setKillConfirmDialog({ sessionId, sessionName });
  };

  const confirmKillSession = () => {
    if (!killConfirmDialog) return;

    const { sessionId } = killConfirmDialog;
    console.log('[KILL] Confirmed, killing session:', sessionId);
    session.sendMessage({ type: 'kill', data: sessionId });
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
        sessions={session.sessions}
        activeSessionId={session.activeSessionId}
        onSelectSession={session.setActiveSessionId}
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
          ref={session.refs.terminalRef}
          style={{
            flex: 1,
            minHeight: 0,
            overflowX: 'auto',
            overflowY: 'hidden',
            paddingRight: '24px',
          }}
        />
        <div
          ref={session.refs.customScrollbarRef}
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
            ref={session.refs.customScrollbarContentRef}
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
