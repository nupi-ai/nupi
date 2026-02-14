import { ToolIcon } from './ToolIcon';
import type { Session } from '../types/session';
import { isSessionActive, truncatePath, getSessionDisplayName, getFullCommand } from '../utils/sessionHelpers';

interface SessionTabsProps {
  sessions: Session[];
  activeSessionId: string | null;
  onSelectSession: (id: string) => void;
  onKillSession: (id: string, name: string) => void;
  onPlayRecording?: (sessionId: string) => void;
}

export function SessionTabs({ sessions, activeSessionId, onSelectSession, onKillSession, onPlayRecording }: SessionTabsProps) {
  return (
    <div style={{
      display: 'flex',
      borderBottom: '1px solid #333',
      backgroundColor: '#2d2d2d', // Darker background for tab bar
      minHeight: '48px', // Use minHeight to accommodate border
      overflowX: 'auto',
      overflowY: 'hidden', // Hide vertical scrollbar
      padding: '8px 8px 0 8px',
      gap: '4px',
      alignItems: 'flex-end',
    }}>
      {sessions.length === 0 ? (
        <div style={{
          padding: '0 16px',
          color: '#666',
          fontSize: '13px',
          display: 'flex',
          alignItems: 'center',
          height: '100%'
        }}>
          No active sessions - start with: ./nupi run &lt;command&gt;
        </div>
      ) : (
        sessions.map(session => {
          const active = isSessionActive(session);
          return (
            <div
              key={session.id}
              onClick={() => onSelectSession(session.id)}
              title={`Session ID: ${session.id}\nCommand: ${getFullCommand(session)}${session.work_dir ? `\nPath: ${session.work_dir}` : ''}`}
              style={{
                padding: '8px 16px',
                boxSizing: 'border-box',
                borderTop: active ? '3px solid #4ade80' : '3px solid #ef4444',
                borderBottom: activeSessionId === session.id ? '1px solid #262626' : '1px solid #333',
                borderLeft: '1px solid #333',
                borderRight: '1px solid #333',
                backgroundColor: activeSessionId === session.id ? '#262626' : '#1a1a1a',
                color: activeSessionId === session.id ?
                  (active ? '#fff' : '#999') : '#999',
                cursor: 'pointer',
                fontFamily: 'system-ui, -apple-system, sans-serif',
                minWidth: '120px',
                maxWidth: '250px',
                height: 'fit-content',
                position: 'relative',
                display: 'flex',
                flexDirection: 'column',
                alignItems: 'flex-start',
                textAlign: 'left',
                opacity: active ? 1 : 0.6,
                borderRadius: '6px 6px 0 0',
                marginBottom: '-1px', // Overlap with container border
                transition: 'all 0.2s ease',
              }}
            >
              {/* First line: Tool icon + name or command + kill button */}
              <div style={{
                display: 'flex',
                alignItems: 'center',
                fontSize: '13px',
                fontWeight: 500,
                marginBottom: '2px',
                width: '100%',
                gap: '6px',
              }}>
                {session.tool && (
                  <ToolIcon
                    toolName={session.tool}
                    iconData={session.tool_icon_data}
                    size={14}
                  />
                )}
                <span style={{
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                  flex: 1,
                }}>
                  {getSessionDisplayName(session)}
                  {!active && ' (inactive)'}
                </span>
                {/* Kill button - only show for active sessions */}
                {active && (
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      e.preventDefault();
                      onKillSession(session.id, getSessionDisplayName(session));
                    }}
                    title="Kill session"
                    style={{
                      background: 'transparent',
                      border: 'none',
                      color: '#999',
                      cursor: 'pointer',
                      padding: '2px 4px',
                      fontSize: '14px',
                      lineHeight: '14px',
                      display: 'flex',
                      alignItems: 'center',
                      justifyContent: 'center',
                      borderRadius: '3px',
                      transition: 'all 0.15s ease',
                    }}
                    onMouseEnter={(e) => {
                      e.currentTarget.style.background = 'rgba(239, 68, 68, 0.2)';
                      e.currentTarget.style.color = '#ef4444';
                    }}
                    onMouseLeave={(e) => {
                      e.currentTarget.style.background = 'transparent';
                      e.currentTarget.style.color = '#999';
                    }}
                  >
                    ✕
                  </button>
                )}
                {/* Play button - only show for inactive sessions */}
                {!active && onPlayRecording && (
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      e.preventDefault();
                      onPlayRecording(session.id);
                    }}
                    title="Play recording"
                    style={{
                      background: 'transparent',
                      border: 'none',
                      color: '#999',
                      cursor: 'pointer',
                      padding: '2px 4px',
                      fontSize: '14px',
                      lineHeight: '14px',
                      display: 'flex',
                      alignItems: 'center',
                      justifyContent: 'center',
                      borderRadius: '3px',
                      transition: 'all 0.15s ease',
                    }}
                    onMouseEnter={(e) => {
                      e.currentTarget.style.background = 'rgba(74, 222, 128, 0.2)';
                      e.currentTarget.style.color = '#4ade80';
                    }}
                    onMouseLeave={(e) => {
                      e.currentTarget.style.background = 'transparent';
                      e.currentTarget.style.color = '#999';
                    }}
                  >
                    ▶
                  </button>
                )}
              </div>

              {/* Second line: Working directory */}
              {session.work_dir && (
                <div style={{
                  fontSize: '11px',
                  opacity: 0.7,
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                  width: '100%',
                }}>
                  {truncatePath(session.work_dir, 35)}
                </div>
              )}
            </div>
          );
        })
      )}
    </div>
  );
}
