import { useEffect, useRef } from 'react';
import { getCurrentWindow } from '@tauri-apps/api/window';
import * as AsciinemaPlayerLibrary from 'asciinema-player';
import 'asciinema-player/dist/bundle/asciinema-player.css';

interface AsciinemaPlayerProps {
  src?: string; // URL to .cast file
  data?: string; // Inline asciicast content (NDJSON)
  autoPlay?: boolean;
  loop?: boolean;
  speed?: number;
  idleTimeLimit?: number;
  theme?: 'asciinema' | 'monokai' | 'solarized-dark' | 'solarized-light';
  fit?: 'width' | 'height' | 'both' | false;
  terminalFontSize?: string;
  terminalLineHeight?: number;
  controls?: boolean | 'auto';
}

export function AsciinemaPlayer({
  src,
  data,
  autoPlay = true,
  loop = false,
  speed = 1,
  idleTimeLimit = 2,
  theme = 'monokai',
  fit = 'both',
  terminalFontSize = '13px',
  terminalLineHeight = 1.33,
  controls = true,
}: AsciinemaPlayerProps) {
  const playerRef = useRef<HTMLDivElement>(null);
  const playerInstance = useRef<any>(null);
  const fullscreenCleanupRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    if (!playerRef.current) return;

    // Use inline data if provided, otherwise fall back to URL
    const source = data ? { data } : src;
    if (!source) return;

    // Create player instance
    // The library supports { data: string } as source but the type definitions only declare string.
    playerInstance.current = AsciinemaPlayerLibrary.create(source as any, playerRef.current, {
      autoPlay,
      loop,
      speed,
      idleTimeLimit,
      theme,
      fit,
      terminalFontSize,
      terminalLineHeight,
      controls,
    });

    // Cleanup on unmount
    return () => {
      if (fullscreenCleanupRef.current) {
        fullscreenCleanupRef.current();
        fullscreenCleanupRef.current = null;
      }
      if (playerInstance.current) {
        // Dispose player if method exists
        if (typeof playerInstance.current.dispose === 'function') {
          playerInstance.current.dispose();
        }
        playerInstance.current = null;
      }
    };
  }, [src, data, autoPlay, loop, speed, idleTimeLimit, theme, fit, terminalFontSize, terminalLineHeight, controls]);

  useEffect(() => {
    if (!playerRef.current) {
      return;
    }

    let cancelled = false;
    let retryId: number | null = null;

    const setupControls = () => {
      if (cancelled || !playerRef.current) {
        return;
      }

      const container = playerRef.current;
      const wrapper = container.querySelector('.ap-wrapper') as HTMLElement | null;
      const nativeButton = container.querySelector('.ap-fullscreen-button') as HTMLElement | null;
      const controlBar = container.querySelector('.ap-control-bar') as HTMLElement | null;

      if (!wrapper || !nativeButton || !controlBar) {
        retryId = window.setTimeout(setupControls, 100);
        return;
      }

      let tauriWindow: ReturnType<typeof getCurrentWindow> | null = null;
      try {
        tauriWindow = getCurrentWindow();
      } catch (error) {
        console.debug('[AsciinemaPlayer] Unable to access current Tauri window', error);
      }

      let isWindowedFullscreen = container.classList.contains('nupi-expanded-container');
      let isNativeFullscreen = !tauriWindow && Boolean(document.fullscreenElement || (document as any).webkitFullscreenElement);

      let createdWindowButton = false;
      const windowButton = (() => {
        const existing = controlBar.querySelector('.ap-windowed-button') as HTMLElement | null;
        if (existing) {
          return existing;
        }

        createdWindowButton = true;

        const button = document.createElement('span');
        button.className = 'ap-button ap-windowed-button ap-tooltip-container';
        button.setAttribute('role', 'button');
        button.setAttribute('tabindex', '0');
        button.setAttribute('aria-label', 'Toggle windowed fullscreen mode');

        const createIcon = (
          className: string,
          paths: Array<string | { d: string; fillRule?: string }>
        ) => {
          const svgNS = 'http://www.w3.org/2000/svg';
          const svg = document.createElementNS(svgNS, 'svg');
          svg.setAttribute('version', '1.1');
          svg.setAttribute('viewBox', '0 0 12 12');
          svg.classList.add('ap-icon', className);
          svg.setAttribute('aria-hidden', 'true');
          for (const spec of paths) {
            const pathData = typeof spec === 'string' ? spec : spec.d;
            const path = document.createElementNS(svgNS, 'path');
            path.setAttribute('d', pathData);
            if (typeof spec === 'object' && spec.fillRule) {
              path.setAttribute('fill-rule', spec.fillRule);
            }
            svg.appendChild(path);
          }
          return svg;
        };

        const icon = createIcon('ap-icon-windowed', [
          {
            d: 'M1 2H11V10H1Z M2 3H10V9H2Z',
            fillRule: 'evenodd',
          },
        ]);
        const tooltip = document.createElement('span');
        tooltip.className = 'ap-tooltip';
        tooltip.textContent = 'Full window (w)';

        button.appendChild(icon);
        button.appendChild(tooltip);

        controlBar.insertBefore(button, nativeButton);

        return button;
      })();

      const iconEnter = nativeButton.querySelector('svg.ap-icon-fullscreen-on') as HTMLElement | null;
      const iconExit = nativeButton.querySelector('svg.ap-icon-fullscreen-off') as HTMLElement | null;
      const windowTooltip = windowButton.querySelector('.ap-tooltip') as HTMLElement | null;

      const applyLayoutState = () => {
        const expanded = isWindowedFullscreen || isNativeFullscreen;
        container.classList.toggle('nupi-expanded-container', expanded);
        wrapper.classList.toggle('nupi-expanded', expanded);
        document.body.classList.toggle('nupi-expanded-body', expanded);
        windowButton.classList.toggle('ap-windowed-active', isWindowedFullscreen);
        windowButton.setAttribute('aria-pressed', String(isWindowedFullscreen));
        if (windowTooltip) {
          windowTooltip.textContent = 'Full window (w)';
        }
      };

      const updateNativeIconVisibility = (active: boolean) => {
        if (iconEnter && iconExit) {
          iconEnter.style.display = active ? 'none' : 'inline';
          iconExit.style.display = active ? 'inline' : 'none';
        }
      };

      const exitWindowedFullscreen = () => {
        if (!isWindowedFullscreen) {
          return;
        }
        isWindowedFullscreen = false;
        applyLayoutState();
      };

      if (tauriWindow) {
        tauriWindow.isFullscreen()
          .then((value) => {
            if (cancelled) return;
            isNativeFullscreen = value;
            if (isNativeFullscreen) {
              exitWindowedFullscreen();
            }
            updateNativeIconVisibility(isNativeFullscreen);
            applyLayoutState();
          })
          .catch((error) => {
            console.warn('[AsciinemaPlayer] Failed to read Tauri fullscreen state', error);
          });
      } else {
        updateNativeIconVisibility(isNativeFullscreen);
        applyLayoutState();
      }

      const enterNativeFullscreen = async () => {
        if (isNativeFullscreen) {
          return;
        }

        try {
          if (tauriWindow) {
            await tauriWindow.setFullscreen(true);
          } else if (wrapper.requestFullscreen) {
            await wrapper.requestFullscreen();
          } else if ((wrapper as any).webkitRequestFullscreen) {
            await (wrapper as any).webkitRequestFullscreen();
          } else if (document.documentElement?.requestFullscreen) {
            await document.documentElement.requestFullscreen();
          } else {
            enterWindowedFullscreen();
            return;
          }
          isNativeFullscreen = true;
          exitWindowedFullscreen();
          updateNativeIconVisibility(true);
          applyLayoutState();
        } catch (error) {
          console.error('[AsciinemaPlayer] Failed to enter native fullscreen', error);
        }
      };

      const exitNativeFullscreen = async () => {
        if (!isNativeFullscreen) {
          return;
        }

        try {
          if (tauriWindow) {
            await tauriWindow.setFullscreen(false);
          } else if (document.exitFullscreen) {
            await document.exitFullscreen();
          }
        } catch (error) {
          console.error('[AsciinemaPlayer] Failed to exit native fullscreen', error);
        } finally {
          isNativeFullscreen = false;
          updateNativeIconVisibility(false);
          applyLayoutState();
        }
      };

      const toggleNativeFullscreen = () => {
        if (isNativeFullscreen) {
          void exitNativeFullscreen();
        } else {
          void enterNativeFullscreen();
        }
      };

      const enterWindowedFullscreen = () => {
        if (isWindowedFullscreen) {
          return;
        }
        isWindowedFullscreen = true;
        if (isNativeFullscreen) {
          void exitNativeFullscreen();
        }
        applyLayoutState();
      };

      const toggleWindowedFullscreen = () => {
        if (isWindowedFullscreen) {
          exitWindowedFullscreen();
        } else {
          enterWindowedFullscreen();
        }
      };

      const onWindowButtonClick = (event: MouseEvent) => {
        event.preventDefault();
        event.stopPropagation();
        toggleWindowedFullscreen();
      };

      const onWindowButtonKeyDown = (event: KeyboardEvent) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          toggleWindowedFullscreen();
        }
      };

      const onNativeButtonClick = (event: MouseEvent) => {
        event.preventDefault();
        event.stopPropagation();
        if (typeof event.stopImmediatePropagation === 'function') {
          event.stopImmediatePropagation();
        }
        toggleNativeFullscreen();
      };

      const onKeyDownCapture = (event: KeyboardEvent) => {
        if (event.defaultPrevented) {
          return;
        }

        const key = event.key.toLowerCase();
        const target = event.target as HTMLElement | null;
        const isInPlayer = target ? container.contains(target) : false;
        const noModifier = !event.altKey && !event.metaKey && !event.ctrlKey;

        if (noModifier && key === 'w' && isInPlayer) {
          event.preventDefault();
          event.stopPropagation();
          toggleWindowedFullscreen();
          return;
        }

        if (noModifier && key === 'f' && isInPlayer) {
          event.preventDefault();
          event.stopPropagation();
          toggleNativeFullscreen();
          return;
        }

        if (key === 'escape') {
          if (isWindowedFullscreen) {
            event.preventDefault();
            event.stopPropagation();
            exitWindowedFullscreen();
          } else if (isNativeFullscreen) {
            event.preventDefault();
            event.stopPropagation();
            void exitNativeFullscreen();
          }
        }
      };

      const onDocumentNativeChange = () => {
        if (tauriWindow) {
          tauriWindow.isFullscreen()
            .then((value) => {
              isNativeFullscreen = value;
              if (isNativeFullscreen) {
                exitWindowedFullscreen();
                updateNativeIconVisibility(true);
              } else {
                updateNativeIconVisibility(false);
              }
              applyLayoutState();
            })
            .catch((error) => {
              console.warn('[AsciinemaPlayer] Failed to check Tauri fullscreen state', error);
            });
        } else {
          const active = Boolean(document.fullscreenElement || (document as any).webkitFullscreenElement);
          isNativeFullscreen = active;
          if (active) {
            exitWindowedFullscreen();
          }
          updateNativeIconVisibility(active);
          applyLayoutState();
        }
      };

      windowButton.addEventListener('click', onWindowButtonClick, true);
      windowButton.addEventListener('keydown', onWindowButtonKeyDown, true);
      nativeButton.addEventListener('click', onNativeButtonClick, true);
      document.addEventListener('keydown', onKeyDownCapture, true);
      document.addEventListener('fullscreenchange', onDocumentNativeChange, true);
      document.addEventListener('webkitfullscreenchange', onDocumentNativeChange as EventListener, true);

      applyLayoutState();

      fullscreenCleanupRef.current = () => {
        windowButton.removeEventListener('click', onWindowButtonClick, true);
        windowButton.removeEventListener('keydown', onWindowButtonKeyDown, true);
        nativeButton.removeEventListener('click', onNativeButtonClick, true);
        document.removeEventListener('keydown', onKeyDownCapture, true);
        document.removeEventListener('fullscreenchange', onDocumentNativeChange, true);
        document.removeEventListener('webkitfullscreenchange', onDocumentNativeChange as EventListener, true);

        isWindowedFullscreen = false;
        isNativeFullscreen = false;
        applyLayoutState();

        if (windowTooltip) {
          windowTooltip.textContent = 'Full window (w)';
        }
        if (iconEnter) {
          iconEnter.style.removeProperty('display');
        }
        if (iconExit) {
          iconExit.style.removeProperty('display');
        }
        if (createdWindowButton && windowButton.parentElement) {
          windowButton.parentElement.removeChild(windowButton);
        }
      };
    };

    setupControls();

    return () => {
      cancelled = true;
      if (retryId !== null) {
        window.clearTimeout(retryId);
      }
      if (fullscreenCleanupRef.current) {
        fullscreenCleanupRef.current();
        fullscreenCleanupRef.current = null;
      }
    };
  }, [src, data]);

  return (
    <div
      ref={playerRef}
      style={{
        width: '100%',
        height: '100%',
        minHeight: '400px',
        position: 'relative',
        isolation: 'isolate'
      }}
    />
  );
}
