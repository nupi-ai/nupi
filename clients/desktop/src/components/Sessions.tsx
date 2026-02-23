import { useState } from 'react';
import { useSession } from '../hooks/useSession';
import { SessionTabs } from './SessionTabs';
import { KillConfirmDialog } from './KillConfirmDialog';
import * as styles from './sessionsStyles';
import '@xterm/xterm/css/xterm.css';

interface SessionsProps {
  onPlayRecording?: (sessionId: string) => void;
}

export function Sessions({ onPlayRecording }: SessionsProps) {
  const session = useSession();
  const [killConfirmDialog, setKillConfirmDialog] = useState<{sessionId: string, sessionName: string} | null>(null);

  // Kill session handlers
  const handleKillSessionClick = (sessionId: string, sessionName: string) => {
    setKillConfirmDialog({ sessionId, sessionName });
  };

  const confirmKillSession = () => {
    if (!killConfirmDialog) return;

    const { sessionId } = killConfirmDialog;
    session.killSession(sessionId);
    setKillConfirmDialog(null);
  };

  const cancelKillSession = () => {
    setKillConfirmDialog(null);
  };

  return (
    <div style={styles.root}>
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
        style={styles.terminalContainer}
      >
        <div
          ref={session.refs.terminalRef}
          style={styles.terminalViewport}
        />
        <div
          ref={session.refs.customScrollbarRef}
          style={styles.customScrollbar}
        >
          <div
            ref={session.refs.customScrollbarContentRef}
            style={styles.customScrollbarContent}
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
