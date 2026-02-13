import { useCallback, useState } from "react";
import { invoke } from "@tauri-apps/api/core";

type JsonValue = unknown;

const CANCELLED_MESSAGE = "Voice stream cancelled by user";

let fallbackOperationCounter = 0;

function createOperationId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  fallbackOperationCounter += 1;
  return `voice-${Date.now()}-${fallbackOperationCounter}-${Math.random().toString(36).slice(2)}`;
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

const MAX_METADATA_ENTRIES = 32;
const MAX_METADATA_KEY_LENGTH = 64;
const MAX_METADATA_VALUE_LENGTH = 512;
const MAX_METADATA_TOTAL_BYTES = 4096;

function normalizeMetadataMap(entries: Record<string, string>): Record<string, string> {
  const normalized: Record<string, string> = Object.create(null) as Record<string, string>;
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

function codePointCount(value: string): number {
  return [...value].length;
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

  let totalBytes = 0;
  for (const [key, value] of pairs) {
    if (codePointCount(key) > MAX_METADATA_KEY_LENGTH) {
      return {
        metadata,
        error: `Metadata key "${key}" exceeds ${MAX_METADATA_KEY_LENGTH} characters`,
      };
    }
    if (codePointCount(value) > MAX_METADATA_VALUE_LENGTH) {
      return {
        metadata,
        error: `Metadata value for "${key}" exceeds ${MAX_METADATA_VALUE_LENGTH} characters`,
      };
    }

    totalBytes += utf8ByteLength(key) + utf8ByteLength(value);
    if (totalBytes > MAX_METADATA_TOTAL_BYTES) {
      return {
        metadata,
        error: `Metadata payload exceeds ${MAX_METADATA_TOTAL_BYTES} bytes in total`,
      };
    }
  }

  return { metadata };
}

export interface VoiceStreamState {
  status: string;
  result: JsonValue;
  isBusy: boolean;
  playbackError: string | null;
  activeOperationId: string | null;
  isCancelling: boolean;
  capabilities: JsonValue;
  performUpload: () => Promise<void>;
  performInterrupt: () => Promise<void>;
  cancelUpload: () => Promise<void>;
  fetchCapabilities: () => Promise<void>;
}

export interface VoiceStreamOptions {
  sessionId: string;
  streamId: string;
  inputPath: string | null;
  outputPath: string | null;
  disablePlayback: boolean;
  metadata: Record<string, string>;
}

export function useVoiceStream(options: VoiceStreamOptions): VoiceStreamState {
  const { sessionId, streamId, inputPath, outputPath, disablePlayback, metadata } = options;

  const [status, setStatus] = useState<string>("Awaiting input");
  const [result, setResult] = useState<JsonValue>(null);
  const [capabilities, setCapabilities] = useState<JsonValue>(null);
  const [isBusy, setBusy] = useState(false);
  const [playbackError, setPlaybackError] = useState<string | null>(null);
  const [activeOperationId, setActiveOperationId] = useState<string | null>(null);
  const [isCancelling, setCancelling] = useState(false);

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
    setStatus("Streaming audio\u2026");
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
    setStatus("Sending interrupt\u2026");
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
    setStatus("Cancelling voice stream\u2026");
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
    setStatus("Fetching audio capabilities\u2026");
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

  return {
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
  };
}
