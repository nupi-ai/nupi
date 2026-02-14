import { useRef, useCallback, useEffect } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import type { Session } from '../types/session';
import { isSessionActive } from '../utils/sessionHelpers';

export function useTerminalInstance(
  activeSessionId: string | null,
  activeSessionRef: React.MutableRefObject<Session | undefined>,
  sendMessage: (msg: object) => void,
) {
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

  // Store sendMessage in ref to avoid stale closures in effects
  const sendMessageRef = useRef(sendMessage);
  sendMessageRef.current = sendMessage;

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

  // Initialize terminal and attach to session when active session changes
  useEffect(() => {
    if (!activeSessionId || !terminalRef.current) return;

    // Get current session status at the time of terminal creation
    const currentSession = activeSessionRef.current;
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
      cleanupTerminal();
      sendMessageRef.current({
        type: 'detach',
        data: attachedSessionId.current,
      });

      console.log('[DETACH] Detaching from:', attachedSessionId.current);
      attachedSessionId.current = null; // Clear immediately after detach

      // Small delay to ensure detach is processed before attach
      setTimeout(attachToNewSession, 50);
    } else {
      // Clean up previous terminal if exists
      cleanupTerminal();
      attachToNewSession();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeSessionId]); // Only re-run when activeSessionId changes
  // We intentionally don't include 'sessions' to avoid re-creating terminal on every status change

  // Cleanup on unmount
  useEffect(() => {
    return () => {
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
  }, []);

  return {
    refs: { terminalRef, customScrollbarRef, customScrollbarContentRef },
    controls: { writeChunk, applyResizeInstructions, markSessionStopped },
    attachedSessionId,
  };
}
