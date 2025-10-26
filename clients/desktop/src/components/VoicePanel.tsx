import { useCallback, useMemo, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { open, save } from "@tauri-apps/plugin-dialog";

type JsonValue = unknown;

const MAX_METADATA_ENTRIES = 32;
const MAX_METADATA_KEY_LENGTH = 64;
const MAX_METADATA_VALUE_LENGTH = 512;
const MAX_METADATA_TOTAL_BYTES = 4096;

const CANCELLED_MESSAGE = "Voice stream cancelled by user";

let fallbackOperationCounter = 0;

function createOperationId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  fallbackOperationCounter += 1;
  return `voice-${Date.now()}-${fallbackOperationCounter}-${Math.random().toString(36).slice(2)}`;
}

function stringify(value: JsonValue): string {
  if (value === null || value === undefined) {
    return "";
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

function normalizeMetadataMap(entries: Record<string, string>): Record<string, string> {
  const normalized: Record<string, string> = {};
  const seen = new Set<string>();
  Object.entries(entries ?? {}).forEach(([rawKey, rawValue]) => {
    const key = rawKey.trim();
    if (!key || seen.has(key)) {
      if (key && seen.has(key)) {
        console.warn(`Duplicate metadata key after normalization ignored: "${key}"`);
      }
      return;
    }
    seen.add(key);
    normalized[key] = (rawValue ?? "").toString().trim();
  });
  return normalized;
}

function utf8ByteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

function validateMetadataMap(
  entries: Record<string, string>,
): { metadata: Record<string, string>; error?: string } {
  const metadata = normalizeMetadataMap(entries);
  const pairs = Object.entries(metadata);

  if (pairs.length > MAX_METADATA_ENTRIES) {
    return {
      metadata,
      error: `Metadata includes ${pairs.length} entries (limit ${MAX_METADATA_ENTRIES})`,
    };
  }

  let total = 0;
  for (const [key, value] of pairs) {
    const keyBytes = utf8ByteLength(key);
    const valueBytes = utf8ByteLength(value);

    if (keyBytes > MAX_METADATA_KEY_LENGTH) {
      return {
        metadata,
        error: `Metadata key “${key}” exceeds ${MAX_METADATA_KEY_LENGTH} characters`,
      };
    }
    if (valueBytes > MAX_METADATA_VALUE_LENGTH) {
      return {
        metadata,
        error: `Metadata value for “${key}” exceeds ${MAX_METADATA_VALUE_LENGTH} characters`,
      };
    }

    total += keyBytes + valueBytes;
    if (total > MAX_METADATA_TOTAL_BYTES) {
      return {
        metadata,
        error: `Metadata payload exceeds ${MAX_METADATA_TOTAL_BYTES} characters in total`,
      };
    }
  }

  return { metadata };
}

function extractPlaybackError(payload: JsonValue): string | null {
  if (payload && typeof payload === "object" && !Array.isArray(payload)) {
    const candidate = (payload as Record<string, unknown>).playback_error;
    if (typeof candidate === "string") {
      const trimmed = candidate.trim();
      if (trimmed.length > 0) {
        return trimmed;
      }
    }
  }
  return null;
}

export function VoicePanel() {
  const [sessionId, setSessionId] = useState("");
  const [streamId, setStreamId] = useState("mic");
  const [inputPath, setInputPath] = useState<string | null>(null);
  const [outputPath, setOutputPath] = useState<string | null>(null);
  const [disablePlayback, setDisablePlayback] = useState(true);
  const [status, setStatus] = useState<string>("Awaiting input");
  const [result, setResult] = useState<JsonValue>(null);
  const [capabilities, setCapabilities] = useState<JsonValue>(null);
  const [isBusy, setBusy] = useState(false);
  const [playbackError, setPlaybackError] = useState<string | null>(null);
  const [activeOperationId, setActiveOperationId] = useState<string | null>(null);
  const [isCancelling, setCancelling] = useState(false);

  const metadata = useMemo(() => ({ client: "desktop" }), []);

  const pickInput = useCallback(async () => {
    setStatus("Opening file picker…");
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
        setStatus("File selection cancelled");
        return;
      }

      setInputPath(selected);
      setStatus(`Selected file: ${selected}`);
    } catch (error) {
      console.error("Failed to open audio picker", error);
      setStatus("Unable to open file picker");
    }
  }, []);

  const pickOutput = useCallback(async () => {
    setStatus("Choosing output file…");
    try {
      const saved = await save({
        defaultPath: "nupi-playback.wav",
        filters: [{ name: "WAV", extensions: ["wav"] }],
      });

      if (saved) {
        setOutputPath(saved);
        setStatus(`Playback audio will be saved to ${saved}`);
        setDisablePlayback(false);
      } else {
        setStatus("Playback output selection cancelled");
      }
    } catch (error) {
      console.error("Failed to open save dialog", error);
      setStatus("Unable to open save dialog");
    }
  }, []);

  const clearOutput = useCallback(() => {
    setOutputPath(null);
    setDisablePlayback(true);
  }, []);

  const performUpload = useCallback(async () => {
    if (!inputPath) {
      setStatus("Select an input audio file first");
      return;
    }
    if (!sessionId.trim()) {
      setStatus("Provide a target session identifier");
      return;
    }

    const metadataValidation = validateMetadataMap(metadata);
    if (metadataValidation.error) {
      setStatus(`Invalid metadata: ${metadataValidation.error}`);
      return;
    }

    setBusy(true);
    setStatus("Streaming audio…");
    setResult(null);
    setPlaybackError(null);
    const operationId = createOperationId();
    setActiveOperationId(operationId);
    try {
      const payload = await invoke<JsonValue>("voice_stream_from_file", {
        sessionId: sessionId.trim(),
        streamId: streamId.trim() || null,
        inputPath,
        playbackOutput: outputPath,
        disablePlayback,
        metadata: metadataValidation.metadata,
        operationId,
      });
      setResult(payload);
      const playbackIssue = extractPlaybackError(payload);
      if (playbackIssue) {
        setPlaybackError(playbackIssue);
        setStatus(`Voice stream completed (playback warning: ${playbackIssue})`);
      } else {
        setPlaybackError(null);
        setStatus("Voice stream completed");
      }
    } catch (error) {
      console.error(error);
      setPlaybackError(null);
      const message = error instanceof Error ? error.message : String(error);
      if (message.trim().toLowerCase() === CANCELLED_MESSAGE.toLowerCase()) {
        setStatus("Voice stream cancelled");
      } else {
        setStatus(`Voice stream failed: ${message}`);
      }
    } finally {
      setBusy(false);
      setActiveOperationId(null);
      setCancelling(false);
    }
  }, [disablePlayback, inputPath, metadata, outputPath, sessionId, streamId]);

  const performInterrupt = useCallback(async () => {
    if (!sessionId.trim()) {
      setStatus("Provide a target session identifier");
      return;
    }

    const metadataValidation = validateMetadataMap(metadata);
    if (metadataValidation.error) {
      setStatus(`Invalid metadata: ${metadataValidation.error}`);
      return;
    }

    setBusy(true);
    setStatus("Sending interrupt…");
    try {
      const payload = await invoke<JsonValue>("voice_interrupt_command", {
        sessionId: sessionId.trim(),
        streamId: streamId.trim() || null,
        metadata: metadataValidation.metadata,
      });
      setResult(payload);
      setPlaybackError(null);
      setStatus("Interrupt sent");
    } catch (error) {
      console.error(error);
      setPlaybackError(null);
      setStatus(
        `Interrupt failed: ${
          error instanceof Error ? error.message : String(error)
        }`
      );
    } finally {
      setBusy(false);
    }
  }, [metadata, sessionId, streamId]);

  const cancelUpload = useCallback(async () => {
    if (!activeOperationId || isCancelling) {
      return;
    }
    setCancelling(true);
    setStatus("Cancelling voice stream…");
    try {
      const cancelled = await invoke<boolean>("voice_cancel_stream", {
        operationId: activeOperationId,
      });
      if (!cancelled) {
        setStatus("No active voice upload to cancel");
        setCancelling(false);
        setBusy(false);
        setActiveOperationId(null);
      }
    } catch (error) {
      console.error(error);
      const message = error instanceof Error ? error.message : String(error);
      setStatus(`Cancel request failed: ${message}`);
      setCancelling(false);
    }
  }, [activeOperationId, isCancelling]);

  const fetchCapabilities = useCallback(async () => {
    setBusy(true);
    setStatus("Fetching audio capabilities…");
    try {
      const payload = await invoke<JsonValue>("voice_status_command", {
        sessionId: sessionId.trim() || null,
      });
      setCapabilities(payload);
      if (
        payload &&
        typeof payload === "object" &&
        payload !== null &&
        "message" in payload &&
        typeof (payload as Record<string, unknown>).message === "string"
      ) {
        setStatus(
          (payload as Record<string, unknown>).message as string
        );
      } else {
        setStatus("Capabilities updated");
      }
    } catch (error) {
      console.error(error);
      setStatus(
        `Failed to fetch capabilities: ${
          error instanceof Error ? error.message : String(error)
        }`
      );
    } finally {
      setBusy(false);
    }
  }, [sessionId]);

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
            {isBusy ? "Streaming…" : "Send Audio"}
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
              {isCancelling ? "Cancelling…" : "Cancel Upload"}
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
{stringify(capabilities) || "Use “Refresh Capabilities” to fetch the latest capture/playback configuration."}
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
