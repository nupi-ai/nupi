import { useState, useEffect } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { Sessions } from "./components/Sessions";
import { History } from "./components/History";
import { AsciinemaPlayer } from "./components/AsciinemaPlayer";
import { VoicePanel } from "./components/VoicePanel";
import "./App.css";

type ViewMode = "sessions" | "history" | "voice";

function App() {
  const [daemonRunning, setDaemonRunning] = useState(false);
  const [daemonStatus, setDaemonStatus] = useState("Starting daemon...");
  const [daemonError, setDaemonError] = useState<string | null>(null);
  const [daemonPort, setDaemonPort] = useState<number | null>(null);
  const [viewMode, setViewMode] = useState<ViewMode>("sessions");
  const [playingRecording, setPlayingRecording] = useState<string | null>(null);

  async function checkDaemonStatus() {
    try {
      const running = await invoke<boolean>("daemon_status");
      setDaemonRunning(running);
      setDaemonStatus(running ? "Daemon is running ✓" : "Daemon is starting...");
      if (running) {
        setDaemonError(null);
      }
    } catch (error) {
      console.error("Failed to check daemon status:", error);
      setDaemonStatus("Failed to check status");
    }
  }

  useEffect(() => {
    // Listen for daemon events from Rust backend
    const unlistenStart = listen("daemon-started", () => {
      setDaemonRunning(true);
      setDaemonStatus("Daemon is running ✓");
      setDaemonError(null);
    });

    const unlistenStop = listen("daemon-stopped", () => {
      setDaemonRunning(false);
      setDaemonStatus("Daemon stopped");
    });

    const unlistenError = listen<string>("daemon-error", (event) => {
      setDaemonError(event.payload);
      setDaemonStatus("Daemon failed to start ✗");
      setDaemonRunning(false);
    });

    // Set up periodic status check
    const interval = setInterval(checkDaemonStatus, 5000); // Check every 5 seconds

    // Initial check after a short delay
    setTimeout(checkDaemonStatus, 1000);

    return () => {
      unlistenStart.then((fn) => fn());
      unlistenStop.then((fn) => fn());
      unlistenError.then((fn) => fn());
      clearInterval(interval);
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    let retryTimer: number | null = null;

    if (!daemonRunning) {
      setDaemonPort(null);
      return;
    }

    const pollPort = async (attempt: number = 1) => {
      retryTimer = null;
      if (cancelled) {
        return;
      }

      try {
        const port = await invoke<number>("get_daemon_port");
        if (!cancelled) {
          console.log(`[App] Daemon port available (attempt ${attempt}):`, port);
          setDaemonPort(port);
        }
      } catch (err) {
        if (cancelled) {
          return;
        }

        console.warn(
          `[App] Waiting for daemon port (attempt ${attempt})`,
          err
        );

        const nextDelay = Math.min(500 * attempt, 5000);
        retryTimer = window.setTimeout(() => pollPort(attempt + 1), nextDelay);
      }
    };

    pollPort();

    return () => {
      cancelled = true;
      if (retryTimer !== null) {
        window.clearTimeout(retryTimer);
      }
    };
  }, [daemonRunning]);

  return (
    <div style={{
      display: "flex",
      flexDirection: "column",
      height: "100vh",
      minHeight: 0,
      overflow: "hidden"
    }}>
      <main style={{
        flex: 1,
        display: 'flex',
        flexDirection: 'column',
        padding: 0,
        minHeight: 0,
        overflow: 'hidden'
      }}>
        {/* Only show Sessions if daemon is running */}
        {daemonRunning ? (
          <>
            {/* Navigation tabs */}
            <div style={{
              display: 'flex',
              backgroundColor: '#1a1a1a',
              borderBottom: '1px solid #333',
              padding: '8px 16px',
              gap: '8px'
            }}>
              <button
                onClick={() => setViewMode('sessions')}
                style={{
                  padding: '6px 16px',
                  backgroundColor: viewMode === 'sessions' ? '#333' : 'transparent',
                  color: viewMode === 'sessions' ? '#fff' : '#999',
                  border: 'none',
                  borderRadius: '4px',
                  cursor: 'pointer',
                  fontSize: '14px',
                  transition: 'all 0.2s'
                }}
              >
                Sessions
              </button>
              <button
                onClick={() => setViewMode('history')}
                style={{
                  padding: '6px 16px',
                  backgroundColor: viewMode === 'history' ? '#333' : 'transparent',
                  color: viewMode === 'history' ? '#fff' : '#999',
                  border: 'none',
                  borderRadius: '4px',
                  cursor: 'pointer',
                  fontSize: '14px',
                  transition: 'all 0.2s'
                }}
              >
                History
              </button>
              <button
                onClick={() => setViewMode('voice')}
                style={{
                  padding: '6px 16px',
                  backgroundColor: viewMode === 'voice' ? '#333' : 'transparent',
                  color: viewMode === 'voice' ? '#fff' : '#999',
                  border: 'none',
                  borderRadius: '4px',
                  cursor: 'pointer',
                  fontSize: '14px',
                  transition: 'all 0.2s'
                }}
              >
                Voice
              </button>
            </div>

            {/* Content */}
            {viewMode === 'sessions' && (
              daemonPort ? (
                <Sessions
                  daemonPort={daemonPort}
                  onPlayRecording={(sessionId) => setPlayingRecording(sessionId)}
                />
              ) : (
                <div className="container" style={{ paddingBottom: "40px" }}>
                  <h1>Nupi Desktop</h1>
                  <p style={{ fontSize: '1.2rem', marginTop: '2rem' }}>
                    Waiting for daemon to publish port...
                  </p>
                  <p style={{ fontSize: '0.9rem', color: '#666', marginTop: '1rem' }}>
                    This usually takes a few seconds after the daemon starts.
                  </p>
                </div>
              )
            )}
            {viewMode === 'history' && daemonPort && (
              <History daemonPort={daemonPort} />
            )}
            {viewMode === 'voice' && (
              <VoicePanel />
            )}
          </>
        ) : (
          <div className="container" style={{ paddingBottom: "40px" }}>
            <h1>Nupi Desktop</h1>
            <p style={{ fontSize: '1.2rem', marginTop: '2rem' }}>
              Waiting for daemon to start...
            </p>
            <p style={{ fontSize: '0.9rem', color: '#666', marginTop: '1rem' }}>
              The daemon will start automatically.
            </p>
          </div>
        )}
      </main>

      {/* Status bar at the bottom */}
      <div style={{
        height: "32px",
        backgroundColor: "#1a1a1a",
        borderTop: "1px solid #333",
        display: "flex",
        alignItems: "center",
        paddingLeft: "12px",
        fontSize: "13px",
        fontFamily: "system-ui, -apple-system, sans-serif",
        flexShrink: 0
      }}>
        <span style={{
          display: "flex",
          alignItems: "center",
          gap: "6px",
          color: "#999"
        }}>
          <span style={{ fontSize: "16px" }}>
            {daemonError ? "🔴" : (daemonRunning ? "🟢" : "🟡")}
          </span>
          <span style={{
            color: daemonError ? "#f87171" : (daemonRunning ? "#4ade80" : "#fbbf24")
          }}>
            {daemonStatus}
          </span>
        </span>
      </div>

      {/* Recording player modal */}
      {playingRecording && daemonPort && (
        <div
          style={{
            position: 'fixed',
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            backgroundColor: 'rgba(0, 0, 0, 0.9)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 1000,
            padding: '32px'
          }}
          onClick={(event) => {
            if (event.target === event.currentTarget) {
              setPlayingRecording(null);
            }
          }}
        >
          <div
            style={{
              width: '90%',
              height: '90%',
              maxWidth: '1200px',
              maxHeight: '800px',
              backgroundColor: '#0d1117',
              borderRadius: '8px',
              display: 'flex',
              flexDirection: 'column'
            }}
          >
            <div style={{
              padding: '12px 16px',
              borderBottom: '1px solid #333',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
              backgroundColor: '#1a1a1a'
            }}>
              <h3 style={{ margin: 0, fontSize: '16px' }}>Recording Playback</h3>
              <button
                onClick={() => setPlayingRecording(null)}
                style={{
                  padding: '4px 12px',
                  backgroundColor: '#333',
                  border: 'none',
                  borderRadius: '4px',
                  color: '#fff',
                  cursor: 'pointer',
                  fontSize: '14px'
                }}
              >
                Close
              </button>
            </div>
            <div style={{
              flex: 1,
              padding: '16px',
              backgroundColor: '#0d1117',
              position: 'relative',
              minHeight: 0
            }}>
              <AsciinemaPlayer
                src={`http://127.0.0.1:${daemonPort}/recordings/${playingRecording}`}
                autoPlay={true}
                loop={false}
                speed={1}
                idleTimeLimit={2}
                theme="monokai"
                fit="both"
                controls={true}
              />
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

export default App;
