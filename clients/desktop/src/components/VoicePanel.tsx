import { useCallback, useMemo, useState } from "react";
import { open, save } from "@tauri-apps/plugin-dialog";
import { useVoiceStream } from "../hooks/useVoiceStream";

function stringify(value: unknown): string {
  if (value === null || value === undefined) {
    return "";
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

export function VoicePanel() {
  const [sessionId, setSessionId] = useState("");
  const [streamId, setStreamId] = useState("mic");
  const [inputPath, setInputPath] = useState<string | null>(null);
  const [outputPath, setOutputPath] = useState<string | null>(null);
  const [disablePlayback, setDisablePlayback] = useState(true);

  const metadata = useMemo(() => ({ client: "desktop" }), []);

  const {
    status,
    result,
    isBusy,
    playbackError,
    activeOperationId,
    isCancelling,
    capabilities,
    performUpload,
    performInterrupt,
    cancelUpload,
    fetchCapabilities,
  } = useVoiceStream({
    sessionId,
    streamId,
    inputPath,
    outputPath,
    disablePlayback,
    metadata,
  });

  const pickInput = useCallback(async () => {
    try {
      const selected = await open({
        multiple: false,
        directory: false,
        filters: [
          { name: "Audio", extensions: ["wav"] },
          { name: "All Files", extensions: ["*"] },
        ],
      });

      if (!selected) {
        return;
      }

      setInputPath(selected);
    } catch (error) {
      console.error("Failed to open audio picker", error);
    }
  }, []);

  const pickOutput = useCallback(async () => {
    try {
      const saved = await save({
        defaultPath: "nupi-playback.wav",
        filters: [{ name: "WAV", extensions: ["wav"] }],
      });

      if (saved) {
        setOutputPath(saved);
        setDisablePlayback(false);
      }
    } catch (error) {
      console.error("Failed to open save dialog", error);
    }
  }, []);

  const clearOutput = useCallback(() => {
    setOutputPath(null);
    setDisablePlayback(true);
  }, []);

  return (
    <div style={{
      display: "flex",
      flexDirection: "column",
      gap: "16px",
      padding: "16px",
      color: "#fff",
      backgroundColor: "#121212",
      overflowY: "auto",
      height: "100%",
    }}>
      <section style={{
        backgroundColor: "#1c1c1c",
        borderRadius: "8px",
        padding: "16px",
        border: "1px solid #2a2a2a",
        boxShadow: "0 4px 12px rgba(0,0,0,0.2)",
      }}>
        <h2 style={{ marginTop: 0 }}>Voice Streaming (Preview)</h2>
        <p style={{ color: "#9ca3af", lineHeight: 1.5 }}>
          This panel provides an early bridge between the desktop app and the new
          voice pipeline. You can attach WAV recordings to an active session,
          forward manual interruptions, and inspect the capabilities reported by
          the daemon. Microphone capture and real-time playback will land in a
          subsequent iteration.
        </p>
        <div style={{
          display: "grid",
          gap: "12px",
          gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))",
          alignItems: "flex-end",
        }}>
          <label style={{ display: "flex", flexDirection: "column", gap: "6px" }}>
            <span style={{ fontSize: "0.85rem", color: "#9ca3af" }}>Session ID</span>
            <input
              value={sessionId}
              onChange={(event) => setSessionId(event.target.value)}
              placeholder="e.g. sess-123"
              style={{
                backgroundColor: "#111",
                color: "#fff",
                border: "1px solid #333",
                borderRadius: "6px",
                padding: "8px 10px",
              }}
            />
          </label>
          <label style={{ display: "flex", flexDirection: "column", gap: "6px" }}>
            <span style={{ fontSize: "0.85rem", color: "#9ca3af" }}>
              Stream ID (optional)
            </span>
            <input
              value={streamId}
              onChange={(event) => setStreamId(event.target.value)}
              placeholder="mic"
              style={{
                backgroundColor: "#111",
                color: "#fff",
                border: "1px solid #333",
                borderRadius: "6px",
                padding: "8px 10px",
              }}
            />
          </label>
        </div>

        <div style={{
          display: "flex",
          flexWrap: "wrap",
          gap: "12px",
          marginTop: "16px",
        }}>
          <button
            onClick={pickInput}
            style={{
              padding: "10px 18px",
              backgroundColor: "#2563eb",
              color: "#fff",
              border: "none",
              borderRadius: "6px",
              cursor: "pointer",
            }}
          >
            {inputPath ? "Change Input File" : "Select Input File"}
          </button>
          <button
            onClick={pickOutput}
            style={{
              padding: "10px 18px",
              backgroundColor: "#1f2937",
              color: "#e5e7eb",
              border: "1px solid #374151",
              borderRadius: "6px",
              cursor: "pointer",
            }}
          >
            {outputPath ? "Change Output Path" : "Save Playback to WAV"}
          </button>
          {outputPath && (
            <button
              onClick={clearOutput}
              style={{
                padding: "10px 18px",
                backgroundColor: "transparent",
                color: "#f87171",
                border: "1px solid #f87171",
                borderRadius: "6px",
                cursor: "pointer",
              }}
            >
              Clear Output
            </button>
          )}
          <label style={{
            display: "flex",
            alignItems: "center",
            gap: "8px",
            color: "#cbd5f5",
          }}>
            <input
              type="checkbox"
              checked={disablePlayback}
              onChange={(event) => setDisablePlayback(event.target.checked)}
              disabled={!!outputPath}
            />
            Skip playback subscription
          </label>
        </div>

        <div style={{ display: "flex", flexWrap: "wrap", gap: "12px", marginTop: "20px" }}>
          <button
            onClick={performUpload}
            disabled={isBusy}
            style={{
              padding: "10px 22px",
              backgroundColor: "#10b981",
              color: "#fff",
              border: "none",
              borderRadius: "6px",
              cursor: isBusy ? "wait" : "pointer",
            }}
          >
            {isBusy ? "Streaming\u2026" : "Send Audio"}
          </button>
          <button
            onClick={performInterrupt}
            disabled={isBusy}
            style={{
              padding: "10px 22px",
              backgroundColor: "#f97316",
              color: "#fff",
              border: "none",
              borderRadius: "6px",
              cursor: isBusy ? "wait" : "pointer",
            }}
          >
            Interrupt TTS
          </button>
          <button
            onClick={fetchCapabilities}
            disabled={isBusy}
            style={{
              padding: "10px 22px",
              backgroundColor: "#4338ca",
              color: "#fff",
              border: "none",
              borderRadius: "6px",
              cursor: isBusy ? "wait" : "pointer",
            }}
          >
            Refresh Capabilities
          </button>
          {isBusy && activeOperationId && (
            <button
              onClick={cancelUpload}
              disabled={isCancelling}
              style={{
                padding: "10px 22px",
                backgroundColor: "#ef4444",
                color: "#fff",
                border: "none",
                borderRadius: "6px",
                cursor: isCancelling ? "wait" : "pointer",
              }}
            >
              {isCancelling ? "Cancelling\u2026" : "Cancel Upload"}
            </button>
          )}
        </div>

        <p style={{ marginTop: "16px", color: "#9ca3af", fontSize: "0.9rem" }}>
          Status: {status}
        </p>
        {inputPath && (
          <p style={{ color: "#6b7280", fontSize: "0.85rem" }}>
            Using audio file: <code>{inputPath}</code>
          </p>
        )}
        {outputPath && (
          <p style={{ color: "#6b7280", fontSize: "0.85rem" }}>
            Playback will be saved to: <code>{outputPath}</code>
          </p>
        )}
        {playbackError && (
          <p style={{ color: "#f87171", fontSize: "0.85rem" }}>
            Playback warning: {playbackError}
          </p>
        )}
      </section>

      <section style={{
        backgroundColor: "#1c1c1c",
        borderRadius: "8px",
        padding: "16px",
        border: "1px solid #2a2a2a",
        boxShadow: "0 4px 12px rgba(0,0,0,0.2)",
      }}>
        <h3 style={{ marginTop: 0 }}>Capabilities</h3>
        <pre style={{
          backgroundColor: "#0f172a",
          padding: "12px",
          borderRadius: "6px",
          fontSize: "0.85rem",
          maxHeight: "220px",
          overflow: "auto",
        }}>
{stringify(capabilities) || "Use \u201cRefresh Capabilities\u201d to fetch the latest capture/playback configuration."}
        </pre>
      </section>

      <section style={{
        backgroundColor: "#1c1c1c",
        borderRadius: "8px",
        padding: "16px",
        border: "1px solid #2a2a2a",
        boxShadow: "0 4px 12px rgba(0,0,0,0.2)",
      }}>
        <h3 style={{ marginTop: 0 }}>Last Command Result</h3>
        <pre style={{
          backgroundColor: "#0f172a",
          padding: "12px",
          borderRadius: "6px",
          fontSize: "0.85rem",
          maxHeight: "240px",
          overflow: "auto",
        }}>
{stringify(result) || "No command executed yet. Run a voice action to see detailed JSON output."}
        </pre>
      </section>
    </div>
  );
}
