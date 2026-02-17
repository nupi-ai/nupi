import { useState, useRef, useCallback, useEffect } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { invoke } from '@tauri-apps/api/core';
import { listen } from '@tauri-apps/api/event';
import type { Session } from '../types/session';
import { isSessionActive } from '../utils/sessionHelpers';

export type { Session };

// Session event payload emitted by the Rust backend
interface SessionEventPayload {
  event_type: string;
  session_id: string;
  data: Record<string, string>;
}

export function useSession() {
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

  // Refs for values needed inside effect closures without re-triggering
  const activeSessionIdRef = useRef(activeSessionId);
  activeSessionIdRef.current = activeSessionId;
  const setActiveSessionIdRef = useRef(setActiveSessionId);
  setActiveSessionIdRef.current = setActiveSessionId;

  // controlsRef: effects read terminal controls without depending on them
  const controlsRef = useRef({
    writeChunk: (_data: string | Uint8Array) => {},
    applyResize: (_cols: number) => {},
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

  const applyResize = useCallback((cols: number) => {
    try {
      updateTerminalGeometry(cols);
    } catch (error) {
      console.error('[Terminal] Failed to apply resize', error);
    }
  }, [updateTerminalGeometry]);

  // Update controlsRef each render so effects always have latest callbacks
  controlsRef.current = { writeChunk, applyResize, markSessionStopped };

  // ──────────────────────────────────────────────────────────
  //  Session management via Tauri IPC
  // ──────────────────────────────────────────────────────────

  const refreshSessions = useCallback(async () => {
    try {
      const list = await invoke<Session[]>('list_sessions');
      const sorted = [...list].sort((a: Session, b: Session) =>
        (a.start_time ? new Date(a.start_time).getTime() : 0) -
        (b.start_time ? new Date(b.start_time).getTime() : 0)
      );
      setSessions(sorted);
      if (sorted.length > 0 && !activeSessionIdRef.current) {
        setActiveSessionIdRef.current(sorted[0].id);
      }
    } catch (err) {
      console.error('[IPC] Failed to list sessions:', err);
    }
  }, []);

  const killSession = useCallback(async (sessionId: string) => {
    try {
      await invoke('kill_session', { sessionId });
    } catch (err) {
      console.error('[IPC] Failed to kill session:', err);
    }
  }, []);

  // ──────────────────────────────────────────────────────────
  //  Global session events effect
  // ──────────────────────────────────────────────────────────

  useEffect(() => {
    // Load initial session list
    refreshSessions();

    // Listen for global session events (session list updates)
    const unlistenPromise = listen<SessionEventPayload>('session_event', (event) => {
      const { event_type, session_id, data } = event.payload;

      switch (event_type) {
        case 'session_created':
          // Refresh full list to get complete session info
          refreshSessions();
          break;

        case 'session_killed':
          setSessions(prev => {
            const remaining = prev.filter(s => s.id !== session_id);
            if (activeSessionIdRef.current === session_id) {
              const nextId = remaining.length > 0 ? remaining[0].id : null;
              activeSessionIdRef.current = nextId;
              setActiveSessionIdRef.current(nextId);
            }
            return remaining;
          });
          break;

        case 'session_status_changed': {
          // Refresh the specific session to get new status
          invoke<Session>('get_session', { sessionId: session_id })
            .then(updated => {
              setSessions(prev => prev.map(s =>
                s.id === session_id ? { ...s, status: updated.status } : s
              ));
            })
            .catch(err => console.error('[IPC] get_session failed:', err));

          // If attached session stopped, mark terminal
          if (session_id === attachedSessionId.current && data?.exit_code !== undefined) {
            controlsRef.current.markSessionStopped();
          }
          break;
        }

        case 'tool_detected': {
          const updates: Partial<Session> = {};
          if (data?.new_tool) updates.tool = data.new_tool;
          setSessions(prev => prev.map(s =>
            s.id === session_id ? { ...s, ...updates } : s
          ));
          break;
        }

        case 'session_mode_changed': {
          if (data?.mode) {
            setSessions(prev => prev.map(s =>
              s.id === session_id ? { ...s, mode: data.mode } : s
            ));
          }
          break;
        }

        case 'resize_instruction': {
          if (session_id === attachedSessionId.current && data?.cols) {
            const cols = parseInt(data.cols, 10);
            if (!isNaN(cols)) {
              controlsRef.current.applyResize(cols);
            }
          }
          break;
        }
      }
    });

    return () => {
      unlistenPromise.then(fn => fn());
    };
  }, [refreshSessions]);

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

    let cancelled = false;
    const unlisteners: Promise<() => void>[] = [];

    // Function to attach to new session
    const attachToNewSession = async () => {
      if (cancelled) return;

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
      attachedSessionId.current = activeSessionId; // Set BEFORE invoking attach

      try {
        await invoke('attach_session', { sessionId: activeSessionId });
      } catch (err) {
        console.error('[IPC] Failed to attach to session:', err);
        return;
      }

      if (cancelled) return;

      // Listen for terminal output from the Rust backend
      unlisteners.push(
        listen<string>(`session_output:${activeSessionId}`, (event) => {
          controlsRef.current.writeChunk(event.payload);
        })
      );

      // Listen for session disconnection
      unlisteners.push(
        listen<void>(`session_disconnected:${activeSessionId}`, () => {
          controlsRef.current.markSessionStopped();
        })
      );

      // Handle terminal input — send to daemon via Tauri IPC
      const encoder = new TextEncoder();
      term.onData((data) => {
        const bytes = Array.from(encoder.encode(data));
        invoke('send_input', { sessionId: activeSessionId, input: bytes })
          .catch(err => console.error('[IPC] send_input failed:', err));
      });

      // Forward terminal resize to daemon so PTY dimensions stay in sync
      term.onResize(({ cols, rows }) => {
        invoke('resize_session', { sessionId: activeSessionId, cols, rows })
          .catch(err => console.error('[IPC] resize_session failed:', err));
      });
    };

    // Detach from previous session if any
    if (attachedSessionId.current && attachedSessionId.current !== activeSessionId) {
      const prevId = attachedSessionId.current;
      cleanupTerminal();
      invoke('detach_session', { sessionId: prevId })
        .catch(err => console.error('[IPC] detach failed:', err));
      attachedSessionId.current = null; // Clear immediately after detach

      // Small delay to ensure detach is processed before attach
      const detachTimer = setTimeout(() => {
        if (!cancelled) attachToNewSession();
      }, 50);

      return () => {
        cancelled = true;
        clearTimeout(detachTimer);
        unlisteners.forEach(p => p.then(fn => fn()));
        cleanupTerminal();
      };
    } else {
      // Clean up previous terminal if exists
      cleanupTerminal();
      attachToNewSession();
    }

    return () => {
      cancelled = true;
      unlisteners.forEach(p => p.then(fn => fn()));
      cleanupTerminal();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeSessionId]); // Only re-run when activeSessionId changes
  // We intentionally don't include 'sessions' to avoid re-creating terminal on every status change

  // Cleanup on unmount — detach from active session
  useEffect(() => {
    return () => {
      if (attachedSessionId.current) {
        invoke('detach_session', { sessionId: attachedSessionId.current })
          .catch(err => console.error('[IPC] cleanup detach failed:', err));
      }
      cleanupTerminal();
    };
  }, []);

  return {
    sessions,
    activeSessionId,
    setActiveSessionId,
    killSession,
    refs: { terminalRef, customScrollbarRef, customScrollbarContentRef },
  };
}
