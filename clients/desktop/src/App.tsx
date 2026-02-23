import { useState, useEffect } from "react";
import { listen } from "@tauri-apps/api/event";
import { api, toErrorMessage } from "./api";
import { Sessions } from "./components/Sessions";
import { History } from "./components/History";
import { AsciinemaPlayer } from "./components/AsciinemaPlayer";
import { VoicePanel } from "./components/VoicePanel";
import * as styles from "./appStyles";
import "./App.css";

type ViewMode = "sessions" | "history" | "voice";

function App() {
  const [daemonRunning, setDaemonRunning] = useState(false);
  const [daemonStatus, setDaemonStatus] = useState("Starting daemon...");
  const [daemonError, setDaemonError] = useState<string | null>(null);
  const [viewMode, setViewMode] = useState<ViewMode>("sessions");
  const [playingRecording, setPlayingRecording] = useState<string | null>(null);
  const [playingCastData, setPlayingCastData] = useState<string | null>(null);
  const [versionMismatch, setVersionMismatch] = useState<string | null>(null);

  async function checkDaemonStatus() {
    try {
      const running = await api.daemon.status();
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

    let dismissTimer: ReturnType<typeof setTimeout> | null = null;

    // Event name must match EVENT_VERSION_MISMATCH in src-tauri/src/lib.rs
    const unlistenVersionMismatch = listen<{ app_version: string; daemon_version: string }>(
      "version-mismatch",
      (event) => {
        const { app_version, daemon_version } = event.payload;
        setVersionMismatch(
          `Version mismatch: app v${app_version}, daemon v${daemon_version} — please restart the daemon or reinstall`
        );
        // Auto-dismiss after 30 seconds
        if (dismissTimer) clearTimeout(dismissTimer);
        dismissTimer = setTimeout(() => setVersionMismatch(null), 30000);
      }
    );

    // Set up periodic status check
    const interval = setInterval(checkDaemonStatus, 5000); // Check every 5 seconds

    // Initial check after a short delay
    const initialCheck = setTimeout(checkDaemonStatus, 1000);

    return () => {
      unlistenStart.then((fn) => fn());
      unlistenStop.then((fn) => fn());
      unlistenError.then((fn) => fn());
      unlistenVersionMismatch.then((fn) => fn());
      clearInterval(interval);
      clearTimeout(initialCheck);
      if (dismissTimer) clearTimeout(dismissTimer);
    };
  }, []);

  async function handlePlayRecording(sessionId: string) {
    setPlayingRecording(sessionId);
    setPlayingCastData(null);
    try {
      const data = await api.recordings.get(sessionId);
      setPlayingCastData(data);
    } catch (err) {
      console.error("[App] Failed to load recording:", toErrorMessage(err));
      setPlayingRecording(null);
    }
  }

  return (
    <div style={styles.root}>
      <main style={styles.main}>
        {/* Only show content if daemon is running */}
        {daemonRunning ? (
          <>
            {/* Navigation tabs */}
            <div style={styles.navTabs}>
              <button
                onClick={() => setViewMode("sessions")}
                style={styles.navButton(viewMode === "sessions")}
              >
                Sessions
              </button>
              <button
                onClick={() => setViewMode("history")}
                style={styles.navButton(viewMode === "history")}
              >
                History
              </button>
              <button
                onClick={() => setViewMode("voice")}
                style={styles.navButton(viewMode === "voice")}
              >
                Voice
              </button>
            </div>

            {/* Content */}
            {viewMode === "sessions" && <Sessions onPlayRecording={handlePlayRecording} />}
            {viewMode === "history" && <History />}
            {viewMode === "voice" && <VoicePanel />}
          </>
        ) : (
          <div className="container" style={styles.waitingContainer}>
            <h1>Nupi Desktop</h1>
            <p style={styles.waitingTitle}>
              Waiting for daemon to start...
            </p>
            <p style={styles.waitingDescription}>
              The daemon will start automatically.
            </p>
          </div>
        )}
      </main>

      {/* Status bar at the bottom */}
      <div style={styles.statusBar}>
        <span style={styles.statusLabelGroup}>
          <span style={styles.statusIcon}>
            {daemonError ? "🔴" : daemonRunning ? "🟢" : "🟡"}
          </span>
          <span style={styles.statusText(!!daemonError, daemonRunning)}>
            {daemonStatus}
          </span>
        </span>
      </div>

      {/* Version mismatch toast */}
      {versionMismatch && (
        <div
          style={styles.versionToast}
        >
          <span style={styles.versionToastIcon}>&#9888;</span>
          <span style={styles.versionToastMessage}>{versionMismatch}</span>
          <button
            onClick={() => setVersionMismatch(null)}
            style={styles.versionToastClose}
          >
            &#10005;
          </button>
        </div>
      )}

      {/* Recording player modal */}
      {playingRecording && (
        <div
          style={styles.recordingOverlay}
          onClick={(event) => {
            if (event.target === event.currentTarget) {
              setPlayingRecording(null);
              setPlayingCastData(null);
            }
          }}
        >
          <div
            style={styles.recordingModal}
          >
            <div style={styles.recordingHeader}>
              <h3 style={styles.recordingTitle}>Recording Playback</h3>
              <button
                onClick={() => {
                  setPlayingRecording(null);
                  setPlayingCastData(null);
                }}
                style={styles.recordingCloseButton}
              >
                Close
              </button>
            </div>
            <div style={styles.recordingContent}>
              {playingCastData ? (
                <AsciinemaPlayer
                  data={playingCastData}
                  autoPlay={true}
                  loop={false}
                  speed={1}
                  idleTimeLimit={2}
                  theme="monokai"
                  fit="both"
                  controls={true}
                />
              ) : (
                <div style={styles.recordingLoadingText}>Loading recording...</div>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

export default App;
