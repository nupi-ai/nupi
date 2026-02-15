import { useState, useRef, useCallback, useEffect } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { parseBinaryFrame, BINARY_FRAME_OUTPUT } from '../utils/protocol';
import { parseServerMessage } from '../types/messages';
import type { Session } from '../types/session';
import { isSessionActive } from '../utils/sessionHelpers';

export type { Session };

const BACKOFF_INITIAL = 500;
const BACKOFF_MAX = 10_000;

export function useSession(daemonPort: number | null) {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null);

  // --- Terminal refs ---
  const terminalRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<Terminal | null>(null);
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
  const attachedSessionId = useRef<string | null>(null);

  // --- Socket refs ---
  const wsRef = useRef<WebSocket | null>(null);

  // Stable ref for sendMessage — closures in terminal onData read this
  const sendMessageRef = useRef<(msg: object) => void>(() => {});

  // Refs for values needed inside socket effect closures without re-triggering
  const activeSessionIdRef = useRef(activeSessionId);
  activeSessionIdRef.current = activeSessionId;
  const setActiveSessionIdRef = useRef(setActiveSessionId);
  setActiveSessionIdRef.current = setActiveSessionId;

  // controlsRef: socket effect reads terminal controls without depending on them
  const controlsRef = useRef({
    writeChunk: (_data: string | Uint8Array) => {},
    applyResizeInstructions: (_instructions: any[]) => {},
    markSessionStopped: () => {},
  });

  // Compute active session from sessions list (via ref, not as dep)
  const activeSessionRef = useRef<Session | undefined>(undefined);
  activeSessionRef.current = sessions.find(s => s.id === activeSessionId);

  // ──────────────────────────────────────────────────────────
  //  Terminal callbacks
  // ──────────────────────────────────────────────────────────

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

  const applyResizeInstructions = useCallback((instructions: any[]) => {
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
  }, [updateTerminalGeometry]);

  const writeChunk = useCallback((data: string | Uint8Array) => {
    if (!xtermRef.current) {
      return;
    }

    xtermRef.current.write(data);
    syncCustomScrollbar();
  }, [syncCustomScrollbar]);

  const markSessionStopped = useCallback(() => {
    if (xtermRef.current) {
      (xtermRef.current as any).options.disableStdin = true;
      (xtermRef.current as any).options.cursorBlink = false;
    }
  }, []);

  // Update controlsRef each render so socket effect always has latest callbacks
  controlsRef.current = { writeChunk, applyResizeInstructions, markSessionStopped };

  // ──────────────────────────────────────────────────────────
  //  Stable sendMessage
  // ──────────────────────────────────────────────────────────

  const sendMessage = useCallback((msg: object) => {
    if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg));
    }
  }, []);

  // Keep sendMessageRef in sync for terminal onData closures
  sendMessageRef.current = sendMessage;

  // ──────────────────────────────────────────────────────────
  //  WebSocket effect [daemonPort]
  // ──────────────────────────────────────────────────────────

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
          controlsRef.current.writeChunk(frame.payload);
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

        case 'session_killed': {
          console.log('[WS] Session killed:', msg.sessionId);
          if (!msg.sessionId) break;
          setSessions(prev => {
            const remaining = prev.filter(s => s.id !== msg.sessionId);
            if (activeSessionIdRef.current === msg.sessionId) {
              const nextId = remaining.length > 0 ? remaining[0].id : null;
              activeSessionIdRef.current = nextId;
              setActiveSessionIdRef.current(nextId);
            }
            return remaining;
          });
          break;
        }

        case 'session_status_changed': {
          if (!msg.sessionId || !msg.data) break;
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, status: msg.data } : s
          ));

          if (msg.sessionId === attachedSessionId.current && msg.data === 'stopped') {
            controlsRef.current.markSessionStopped();
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
          const updates: Partial<Session> = {};
          if ('tool' in msg.data) updates.tool = msg.data.tool;
          if ('tool_icon' in msg.data) updates.tool_icon = msg.data.tool_icon;
          if ('tool_icon_data' in msg.data) updates.tool_icon_data = msg.data.tool_icon_data;
          setSessions(prev => prev.map(s =>
            s.id === msg.sessionId ? { ...s, ...updates } : s
          ));
          break;
        }

        case 'resize_instruction': {
          if (msg.sessionId === attachedSessionId.current && msg.data && Array.isArray(msg.data.instructions)) {
            controlsRef.current.applyResizeInstructions(msg.data.instructions);
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
      ws.binaryType = 'arraybuffer';

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

  // ──────────────────────────────────────────────────────────
  //  Terminal cleanup helper
  // ──────────────────────────────────────────────────────────

  // Helper: clean up terminal, observers, and scrollbar listeners.
  // Only accesses refs — no need for useCallback since identity stability isn't required.
  const cleanupTerminal = () => {
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
  };

  // ──────────────────────────────────────────────────────────
  //  Terminal effect [activeSessionId]
  // ──────────────────────────────────────────────────────────

  // Initialize terminal and attach to session when active session changes
  useEffect(() => {
    if (!activeSessionId || !terminalRef.current) return;

    // Function to attach to new session
    const attachToNewSession = () => {
      // Read session status at attachment time (not at effect start) to avoid
      // stale values when called after the 50ms detach delay
      const currentSession = activeSessionRef.current;
      const isActive = currentSession && isSessionActive(currentSession);

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
      console.log('[ATTACH] Attaching to session:', activeSessionId, isActive ? '(active)' : '(inactive)');
      attachedSessionId.current = activeSessionId; // Set BEFORE sending attach
      sendMessageRef.current({
        type: 'attach',
        data: activeSessionId,
      });

      // Handle terminal input - send to daemon
      // Daemon will validate if session is active
      term.onData((data) => {
        sendMessageRef.current({
          type: 'input',
          data: {
            sessionId: activeSessionId,
            input: data, // Send as plain string
          },
        });
      });
    };

    let detachTimer: ReturnType<typeof setTimeout> | null = null;

    // Detach from previous session if any
    if (attachedSessionId.current && attachedSessionId.current !== activeSessionId) {
      cleanupTerminal();
      sendMessageRef.current({
        type: 'detach',
        data: attachedSessionId.current,
      });

      console.log('[DETACH] Detaching from:', attachedSessionId.current);
      attachedSessionId.current = null; // Clear immediately after detach

      // Small delay to ensure detach is processed before attach
      detachTimer = setTimeout(attachToNewSession, 50);
    } else {
      // Clean up previous terminal if exists
      cleanupTerminal();
      attachToNewSession();
    }

    return () => {
      if (detachTimer !== null) {
        clearTimeout(detachTimer);
      }
      cleanupTerminal();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeSessionId]); // Only re-run when activeSessionId changes
  // We intentionally don't include 'sessions' to avoid re-creating terminal on every status change

  // Cleanup on unmount
  useEffect(() => {
    return () => cleanupTerminal();
  }, []);

  return {
    sessions,
    activeSessionId,
    setActiveSessionId,
    sendMessage,
    refs: { terminalRef, customScrollbarRef, customScrollbarContentRef },
  };
}
