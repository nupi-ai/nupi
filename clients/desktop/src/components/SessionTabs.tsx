import { useState } from 'react';
import { ToolIcon } from './ToolIcon';
import type { Session } from '../types/session';
import { isSessionActive, truncatePath, getSessionDisplayName, getFullCommand } from '../utils/sessionHelpers';
import { mergeStyles } from '../utils/mergeStyles';
import * as styles from './sessionTabsStyles';

interface SessionTabsProps {
  sessions: Session[];
  activeSessionId: string | null;
  onSelectSession: (id: string) => void;
  onKillSession: (id: string, name: string) => void;
  onPlayRecording?: (sessionId: string) => void;
}

export function SessionTabs({ sessions, activeSessionId, onSelectSession, onKillSession, onPlayRecording }: SessionTabsProps) {
  const [hoveredActionKey, setHoveredActionKey] = useState<string | null>(null);

  return (
    <div style={styles.tabsContainer}>
      {sessions.length === 0 ? (
        <div style={styles.emptyStateText}>
          No active sessions - start with: ./nupi run &lt;command&gt;
        </div>
      ) : (
        sessions.map(session => {
          const active = isSessionActive(session);
          const isSelected = activeSessionId === session.id;
          const killHoverKey = `${session.id}:kill`;
          const playHoverKey = `${session.id}:play`;
          return (
            <div
              key={session.id}
              onClick={() => onSelectSession(session.id)}
              title={`Session ID: ${session.id}\nCommand: ${getFullCommand(session)}${session.work_dir ? `\nPath: ${session.work_dir}` : ''}`}
              style={styles.sessionTab(active, isSelected)}
            >
              {/* First line: Tool icon + name or command + kill button */}
              <div style={styles.tabHeaderRow}>
                {session.tool && (
                  <ToolIcon
                    toolName={session.tool}
                    iconData={session.tool_icon_data}
                    size={14}
                  />
                )}
                <span style={styles.tabLabel}>
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
                    style={mergeStyles(
                      styles.tabActionButton,
                      hoveredActionKey === killHoverKey && styles.killButtonHover,
                    )}
                    onMouseEnter={() => setHoveredActionKey(killHoverKey)}
                    onMouseLeave={() => setHoveredActionKey(null)}
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
                    style={mergeStyles(
                      styles.tabActionButton,
                      hoveredActionKey === playHoverKey && styles.playButtonHover,
                    )}
                    onMouseEnter={() => setHoveredActionKey(playHoverKey)}
                    onMouseLeave={() => setHoveredActionKey(null)}
                  >
                    ▶
                  </button>
                )}
              </div>

              {/* Second line: Working directory */}
              {session.work_dir && (
                <div style={styles.workingDirectoryText}>
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
