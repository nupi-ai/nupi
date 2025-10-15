import { useState, useEffect, useRef, useCallback } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { ToolIcon } from './ToolIcon';
import '@xterm/xterm/css/xterm.css';

interface Session {
  id: string;
  command: string;
  args?: string[];
  work_dir?: string;
  tool?: string;
  tool_icon?: string;
  tool_icon_data?: string; // Base64 encoded icon data
  status: string;
  start_time: string;
  pid?: number;
  mode?: string;
}

const BINARY_MAGIC = 0xbf;
const BINARY_FRAME_OUTPUT = 0x01;

// Check if session is active (process is alive - running or detached)
// For Tauri UI, we treat both 'running' and 'detached' as active
// because attaching to a detached session makes it running anyway
function isSessionActive(session: Session): boolean {
  return session.status === 'running' || session.status === 'detached';
}

// Truncate path showing the end with ellipsis at the beginning
function truncatePath(path: string | undefined, maxLength: number = 40): string {
  if (!path) return '';
  if (path.length <= maxLength) return path;
  return '...' + path.slice(-(maxLength - 3));
}

// Get display name for the session (tool name or full command with args)
function getSessionDisplayName(session: Session): string {
  // If we have a detected tool, show just the tool name
  if (session.tool) {
    return session.tool;
  }
  // Otherwise show full command with arguments
  return getFullCommand(session);
}

// Get full command string with arguments
function getFullCommand(session: Session): string {
  if (session.args && session.args.length > 0) {
    return `${session.command} ${session.args.join(' ')}`;
  }
  return session.command;
}

interface SessionsProps {
  daemonPort: number | null;
  onPlayRecording?: (sessionId: string) => void;
}

export function Sessions({ daemonPort, onPlayRecording }: SessionsProps) {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null);
  const [killConfirmDialog, setKillConfirmDialog] = useState<{sessionId: string, sessionName: string} | null>(null);
  const terminalRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<Terminal | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const attachedSessionId = useRef<string | null>(null);
  const fitAddonRef = useRef<FitAddon | null>(null);
  const resizeObserverRef = useRef<ResizeObserver | null>(null);
  const remoteColsRef = useRef<number | null>(null);
  const viewportRef = useRef<HTMLElement | null>(null);
  const customScrollbarRef = useRef<HTMLDivElement | null>(null);
  const customScrollbarContentRef = useRef<HTMLDivElement | null>(null);
  const customScrollbarCleanupRef = useRef<(() => void)[] | null>(null);
  const isSyncingFromViewport = useRef(false);
  const isSyncingFromCustom = useRef(false);
  const syncRafId = useRef<number | null>(null);

  const handleKillSessionClick = (sessionId: string, sessionName: string) => {
    console.log('[KILL] Button clicked!', sessionId, sessionName);
    setKillConfirmDialog({ sessionId, sessionName });
  };

  const confirmKillSession = () => {
    if (!killConfirmDialog) return;

    const { sessionId } = killConfirmDialog;
    console.log('[KILL] Confirmed, killing session:', sessionId);

    // Send kill command directly to daemon via WebSocket
    if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      console.log('[KILL] Sending kill via WebSocket');
      wsRef.current.send(JSON.stringify({
        type: 'kill',
        data: sessionId,
      }));
      console.log(`[KILL] Sent kill command for session: ${sessionId}`);
    } else {
      console.error(`[KILL] WebSocket not connected (state: ${wsRef.current?.readyState})`);
    }

    setKillConfirmDialog(null);
  };

  const cancelKillSession = () => {
    console.log('[KILL] Cancelled');
    setKillConfirmDialog(null);
  };

  const syncCustomScrollbar = useCallback(() => {
    const viewport = viewportRef.current;
    const custom = customScrollbarRef.current;
    const track = customScrollbarContentRef.current;

    if (!viewport || !custom || !track) {
      return;
    }

    const scrollHeight = viewport.scrollHeight;
    const clientHeight = viewport.clientHeight;
    const needsScrollbar = scrollHeight > clientHeight + 1;

    track.style.height = `${scrollHeight}px`;
    custom.style.visibility = needsScrollbar ? 'visible' : 'hidden';
    custom.style.pointerEvents = needsScrollbar ? 'auto' : 'none';

    isSyncingFromViewport.current = true;
    custom.scrollTop = viewport.scrollTop;
    isSyncingFromViewport.current = false;
  }, []);

  const updateTerminalGeometry = useCallback((cols?: number) => {
    const term = xtermRef.current;
    const fitAddon = fitAddonRef.current;

    if (!term || !fitAddon) {
      return;
    }

    if (typeof cols === 'number') {
      remoteColsRef.current = cols;
    }

    const viewport = term.element?.querySelector('.xterm-viewport') as HTMLElement | null;
    const nearBottom = !viewport
      || viewport.scrollTop >= viewport.scrollHeight - viewport.clientHeight - 4;

    try {
      fitAddon.fit();

      const fittedRows = term.rows;
      const desiredCols = remoteColsRef.current ?? term.cols;

      if (remoteColsRef.current != null && term.cols !== desiredCols) {
        term.resize(desiredCols, fittedRows);
      }

      if (nearBottom) {
        // Use requestAnimationFrame to ensure scroll happens after DOM updates
        requestAnimationFrame(() => {
          term.scrollToBottom();
          syncCustomScrollbar();
        });
      } else {
        // Sync immediately if not scrolling
        syncCustomScrollbar();
      }
    } catch (error) {
      console.error('[Terminal] Failed to resize', error);
      syncCustomScrollbar();
    }
  }, [syncCustomScrollbar]);

  const applyResizeInstructions = (instructions: any[]) => {
    if (!xtermRef.current) {
      return;
    }

    for (const instruction of instructions) {
      const size = instruction?.Size ?? instruction?.size;
      if (!size) {
        continue;
      }

      const cols = size?.Cols ?? size?.cols;
      try {
        updateTerminalGeometry(typeof cols === 'number' ? cols : undefined);
      } catch (error) {
        console.error('[Terminal] Failed to apply resize instruction', error);
      }
    }
  };

  const writeChunk = (data: string | Uint8Array) => {
    if (!xtermRef.current) {
      return;
    }

    const term = xtermRef.current as Terminal & {
      writeUtf8?: (data: Uint8Array) => void;
    };

    if (data instanceof Uint8Array) {
      term.write(data);
    } else {
      term.write(data);
    }

    syncCustomScrollbar();
  };

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

    const textDecoder = new TextDecoder();

    const handleBinaryMessage = (buffer: ArrayBuffer) => {
      if (buffer.byteLength < 4) {
        return;
      }

      const view = new DataView(buffer);
      if (view.getUint8(0) !== BINARY_MAGIC) {
        return;
      }

      const frameType = view.getUint8(1);
      const sessionIdLength = view.getUint16(2, true);
      const headerSize = 4;

      if (buffer.byteLength < headerSize + sessionIdLength) {
        return;
      }

      let sessionId = '';
      try {
        const idBytes = new Uint8Array(buffer, headerSize, sessionIdLength);
        sessionId = textDecoder.decode(idBytes);
      } catch (err) {
        console.error('[WS] Failed to decode session id from binary frame', err);
        return;
      }

      const payloadOffset = headerSize + sessionIdLength;
      if (payloadOffset > buffer.byteLength) {
        return;
      }

      const payload = new Uint8Array(buffer, payloadOffset);

      switch (frameType) {
        case BINARY_FRAME_OUTPUT: {
          if (sessionId !== attachedSessionId.current) {
            return;
          }
          if (payload.length === 0) {
            return;
          }
          writeChunk(payload);
          break;
        }
        default:
          console.warn('[WS] Unknown binary frame type:', frameType);
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
          const sortedSessions = [...msg.data].sort((a, b) =>
            new Date(a.start_time).getTime() - new Date(b.start_time).getTime()
          );
          setSessions(sortedSessions);
          if (sortedSessions.length > 0 && !activeSessionId) {
            setActiveSessionId(sortedSessions[0].id);
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
            const sorted = newSessions.sort((a, b) =>
              new Date(a.start_time).getTime() - new Date(b.start_time).getTime()
            );

            if (!activeSessionId && sorted.length === 1) {
              setActiveSessionId(sorted[0].id);
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
            if (xtermRef.current) {
              (xtermRef.current as any).options.disableStdin = true;
              (xtermRef.current as any).options.cursorBlink = false;
              // Don't write anything - leave terminal output as-is
            }
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
            applyResizeInstructions(msg.data.instructions);
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

  // Initialize terminal and attach to session when active session changes
  useEffect(() => {
    if (!activeSessionId || !terminalRef.current || !wsRef.current) return;

    // Get current session status at the time of terminal creation
    const currentSession = sessions.find(s => s.id === activeSessionId);
    const isActive = currentSession && isSessionActive(currentSession);

    // Function to attach to new session
    const attachToNewSession = () => {

      // Create new terminal with appropriate settings
      const term = new Terminal({
        cursorBlink: isActive,
        fontSize: 14,
        fontFamily: 'Menlo, Monaco, "Courier New", monospace',
        theme: {
          background: '#262626', // Match terminal container background
          foreground: '#d4d4d4',
        },
        disableStdin: !isActive, // Disable input for inactive sessions
      });

      const fitAddon = new FitAddon();
      term.loadAddon(fitAddon);
      fitAddonRef.current = fitAddon;
      remoteColsRef.current = null;

      if (terminalRef.current) {
        term.open(terminalRef.current);
        terminalRef.current.style.overflowX = 'auto';
        terminalRef.current.style.overflowY = 'hidden';
        terminalRef.current.style.position = 'relative';
        const viewport = terminalRef.current.querySelector('.xterm-viewport') as HTMLElement | null;
        if (viewport) {
          viewport.style.overflowX = 'auto';
          viewport.style.overflowY = 'auto';
          viewport.style.scrollbarWidth = 'none';
          (viewport.style as any).msOverflowStyle = 'none';
          viewportRef.current = viewport;
        }
        const screen = terminalRef.current.querySelector('.xterm-screen') as HTMLElement | null;
        if (screen) {
          screen.style.overflow = 'visible';
        }

        if (resizeObserverRef.current) {
          resizeObserverRef.current.disconnect();
          resizeObserverRef.current = null;
        }

        const observer = new ResizeObserver(() => {
          requestAnimationFrame(() => updateTerminalGeometry());
        });

        observer.observe(terminalRef.current);
        resizeObserverRef.current = observer;

        if (customScrollbarCleanupRef.current) {
          customScrollbarCleanupRef.current.forEach(fn => fn());
          customScrollbarCleanupRef.current = null;
        }

        const cleanupTasks: (() => void)[] = [];

        if (viewportRef.current) {
          const onViewportScroll = () => {
            if (isSyncingFromCustom.current) {
              return;
            }
            if (!customScrollbarRef.current || !viewportRef.current) {
              return;
            }

            // Cancel any pending RAF
            if (syncRafId.current !== null) {
              cancelAnimationFrame(syncRafId.current);
            }

            // Schedule sync in next frame
            syncRafId.current = requestAnimationFrame(() => {
              if (!customScrollbarRef.current || !viewportRef.current) {
                return;
              }
              isSyncingFromViewport.current = true;
              customScrollbarRef.current.scrollTop = viewportRef.current.scrollTop;
              isSyncingFromViewport.current = false;
              syncRafId.current = null;
            });
          };
          viewportRef.current.addEventListener('scroll', onViewportScroll, { passive: true });
          cleanupTasks.push(() => {
            viewportRef.current?.removeEventListener('scroll', onViewportScroll);
            if (syncRafId.current !== null) {
              cancelAnimationFrame(syncRafId.current);
              syncRafId.current = null;
            }
          });
        }

        if (customScrollbarRef.current) {
          const customElement = customScrollbarRef.current;
          const onCustomScroll = () => {
            if (isSyncingFromViewport.current) {
              return;
            }
            if (!viewportRef.current) {
              return;
            }

            // Cancel any pending RAF
            if (syncRafId.current !== null) {
              cancelAnimationFrame(syncRafId.current);
            }

            // Schedule sync in next frame
            syncRafId.current = requestAnimationFrame(() => {
              if (!viewportRef.current) {
                return;
              }
              isSyncingFromCustom.current = true;
              viewportRef.current.scrollTop = customElement.scrollTop;
              isSyncingFromCustom.current = false;
              syncRafId.current = null;
            });
          };
          customElement.addEventListener('scroll', onCustomScroll, { passive: true });
          cleanupTasks.push(() => {
            customElement.removeEventListener('scroll', onCustomScroll);
            if (syncRafId.current !== null) {
              cancelAnimationFrame(syncRafId.current);
              syncRafId.current = null;
            }
          });
        }

        customScrollbarCleanupRef.current = cleanupTasks;
      }

      xtermRef.current = term;
      updateTerminalGeometry();
      syncCustomScrollbar();

      // For active sessions, clear and attach
      // For inactive sessions, we'll get the content via attach which will replay history
      if (isActive) {
        term.clear();
      }

      // Attach to all sessions (both active and inactive) to get history
      if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
        console.log('[ATTACH] Attaching to session:', activeSessionId, isActive ? '(active)' : '(inactive)');
        attachedSessionId.current = activeSessionId; // Set BEFORE sending attach
        wsRef.current.send(JSON.stringify({
          type: 'attach',
          data: activeSessionId,
        }));
      }

      // Handle terminal input - send to daemon
      // Daemon will validate if session is active
      term.onData((data) => {
        if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
          wsRef.current.send(JSON.stringify({
            type: 'input',
            data: {
              sessionId: activeSessionId,
              input: data, // Send as plain string
            },
          }));
        }
      });

      // Return cleanup function
      return () => {
        if (xtermRef.current) {
          xtermRef.current.dispose();
          xtermRef.current = null;
        }
      };
    };

    // Detach from previous session if any
    if (attachedSessionId.current && attachedSessionId.current !== activeSessionId) {
      // Clean up previous terminal
      if (xtermRef.current) {
        xtermRef.current.dispose();
        xtermRef.current = null;
      }
      if (resizeObserverRef.current) {
        resizeObserverRef.current.disconnect();
        resizeObserverRef.current = null;
      }
      fitAddonRef.current = null;
      remoteColsRef.current = null;
      viewportRef.current = null;
      if (customScrollbarCleanupRef.current) {
        customScrollbarCleanupRef.current.forEach(fn => fn());
        customScrollbarCleanupRef.current = null;
      }
      wsRef.current.send(JSON.stringify({
        type: 'detach',
        data: attachedSessionId.current,
      }));

      console.log('[DETACH] Detaching from:', attachedSessionId.current);
      attachedSessionId.current = null; // Clear immediately after detach

      // Small delay to ensure detach is processed before attach
      setTimeout(attachToNewSession, 50);
    } else {
      // Clean up previous terminal if exists
      if (xtermRef.current) {
        xtermRef.current.dispose();
        xtermRef.current = null;
      }
      if (resizeObserverRef.current) {
        resizeObserverRef.current.disconnect();
        resizeObserverRef.current = null;
      }
      fitAddonRef.current = null;
      remoteColsRef.current = null;
      viewportRef.current = null;
      if (customScrollbarCleanupRef.current) {
        customScrollbarCleanupRef.current.forEach(fn => fn());
        customScrollbarCleanupRef.current = null;
      }
      attachToNewSession();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeSessionId]); // Only re-run when activeSessionId changes
  // We intentionally don't include 'sessions' to avoid re-creating terminal on every status change

  useEffect(() => {
    return () => {
      if (resizeObserverRef.current) {
        resizeObserverRef.current.disconnect();
        resizeObserverRef.current = null;
      }
      fitAddonRef.current = null;
      remoteColsRef.current = null;
      viewportRef.current = null;
      if (customScrollbarCleanupRef.current) {
        customScrollbarCleanupRef.current.forEach(fn => fn());
        customScrollbarCleanupRef.current = null;
      }
    };
  }, []);

  // Don't show "no sessions" message immediately - keep WebSocket listening
  // This allows the UI to react when sessions are started

  return (
    <div style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0, overflow: 'hidden' }}>
      {/* Tabs at top */}
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
            onClick={() => setActiveSessionId(session.id)}
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
                    handleKillSessionClick(session.id, getSessionDisplayName(session));
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
          ref={terminalRef}
          style={{
            flex: 1,
            minHeight: 0,
            overflowX: 'auto',
            overflowY: 'hidden',
            paddingRight: '24px',
          }}
        />
        <div
          ref={customScrollbarRef}
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
            ref={customScrollbarContentRef}
            style={{
              width: '100%',
              background: 'transparent',
            }}
          />
        </div>
      </div>

      {/* Kill confirmation dialog */}
      {killConfirmDialog && (
        <div
          style={{
            position: 'fixed',
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            backgroundColor: 'rgba(0, 0, 0, 0.7)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 1000,
          }}
          onClick={cancelKillSession}
        >
          <div
            style={{
              backgroundColor: '#1a1a1a',
              border: '1px solid #333',
              borderRadius: '8px',
              padding: '24px',
              minWidth: '400px',
              maxWidth: '500px',
            }}
            onClick={(e) => e.stopPropagation()}
          >
            <h3 style={{ margin: '0 0 16px 0', color: '#fff', fontSize: '18px' }}>
              Kill Session?
            </h3>
            <p style={{ margin: '0 0 24px 0', color: '#ccc', lineHeight: '1.5' }}>
              Are you sure you want to kill session <strong>"{killConfirmDialog.sessionName}"</strong>?
              <br /><br />
              The session will be stopped but remain visible in the list.
            </p>
            <div style={{ display: 'flex', gap: '12px', justifyContent: 'flex-end' }}>
              <button
                onClick={cancelKillSession}
                style={{
                  padding: '8px 16px',
                  backgroundColor: '#333',
                  color: '#fff',
                  border: 'none',
                  borderRadius: '4px',
                  cursor: 'pointer',
                  fontSize: '14px',
                }}
              >
                Cancel
              </button>
              <button
                onClick={confirmKillSession}
                style={{
                  padding: '8px 16px',
                  backgroundColor: '#ef4444',
                  color: '#fff',
                  border: 'none',
                  borderRadius: '4px',
                  cursor: 'pointer',
                  fontSize: '14px',
                }}
              >
                Kill Session
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
