import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import { Platform, PermissionsAndroid } from "react-native";
import {
  RealtimeTranscriber,
  type RealtimeTranscribeEvent,
} from "whisper.rn/realtime-transcription";
import { AudioPcmStreamAdapter } from "whisper.rn/realtime-transcription/adapters/AudioPcmStreamAdapter";

import {
  deleteModel,
  deleteCoreML,
  downloadCoreML,
  downloadModel,
  isCoreMLDownloaded,
  isModelDownloaded,
  whisperManager,
} from "./whisper";
import { raceTimeout } from "./raceTimeout";

export type ModelStatus =
  | "not_downloaded"
  | "downloading"
  | "initializing"
  | "ready";

export type RecordingStatus =
  | "idle"
  | "recording"
  | "result";

export type DownloadStage = "model" | "coreml" | "extracting" | null;

interface VoiceContextValue {
  modelStatus: ModelStatus;
  recordingStatus: RecordingStatus;
  confirmedText: string;
  pendingText: string;
  voiceError: string | null;
  downloadProgress: number;
  downloadStage: DownloadStage;
  initModel: () => Promise<void>;
  startRecording: () => Promise<void>;
  stopRecording: () => Promise<void>;
  clearTranscription: () => void;
  clearVoiceError: () => void;
}

const VoiceContext = createContext<VoiceContextValue>({
  modelStatus: "not_downloaded",
  recordingStatus: "idle",
  confirmedText: "",
  pendingText: "",
  voiceError: null,
  downloadProgress: 0,
  downloadStage: null,
  initModel: async () => {},
  startRecording: async () => {},
  stopRecording: async () => {},
  clearTranscription: () => {},
  clearVoiceError: () => {},
});

export function useVoice() {
  return useContext(VoiceContext);
}

async function requestMicPermission(): Promise<true | "denied" | "never_ask_again"> {
  if (Platform.OS === "android") {
    const result = await PermissionsAndroid.request(
      PermissionsAndroid.PERMISSIONS.RECORD_AUDIO,
      {
        title: "Microphone Permission",
        message: "Nupi needs microphone access for voice commands.",
        buttonPositive: "OK",
        buttonNegative: "Cancel",
      },
    );
    if (result === PermissionsAndroid.RESULTS.GRANTED) return true;
    if (result === PermissionsAndroid.RESULTS.NEVER_ASK_AGAIN) return "never_ask_again";
    return "denied";
  }
  // iOS: permission is prompted automatically by the native audio module
  // when AudioPcmStreamAdapter.start() is called.
  return true;
}

export function VoiceProvider({ children }: { children: React.ReactNode }) {
  const [modelStatus, setModelStatus] = useState<ModelStatus>("not_downloaded");
  const modelStatusRef = useRef<ModelStatus>("not_downloaded");
  const [recordingStatus, setRecordingStatus] =
    useState<RecordingStatus>("idle");
  const [confirmedText, setConfirmedText] = useState("");
  const [pendingText, setPendingText] = useState("");
  const [voiceError, setVoiceError] = useState<string | null>(null);
  const [downloadProgress, setDownloadProgress] = useState(0);
  const [downloadStage, setDownloadStage] = useState<DownloadStage>(null);
  const destroyedRef = useRef(false);
  const transcriberRef = useRef<RealtimeTranscriber | null>(null);
  const audioStreamRef = useRef<AudioPcmStreamAdapter | null>(null);
  const sliceTextsRef = useRef<Map<number, string>>(new Map());
  const prevFullTextRef = useRef("");
  const stoppedRef = useRef(false);
  // Set by clearTranscription() to prevent async stopRecording() and
  // onTranscribe callbacks from re-populating text state after cleanup.
  const transcriptionClearedRef = useRef(false);
  const idleTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Synchronous re-entry guard for startRecording — prevents two concurrent
  // async calls from both passing the transcriberRef null check and leaking
  // native audio resources.
  const isStartingRecordingRef = useRef(false);
  // Synchronous re-entry guard for stopRecording — prevents concurrent calls
  // (e.g., onPressOut + sessionStopped effect + unmount) from double-stopping
  // the same native transcriber.
  const isStoppingRecordingRef = useRef(false);

  const updateModelStatus = useCallback((status: ModelStatus) => {
    modelStatusRef.current = status;
    setModelStatus(status);
  }, []);

  // Transcription callback — builds confirmed/pending text from slice results
  const onTranscribe = useCallback((event: RealtimeTranscribeEvent) => {
    if (destroyedRef.current || transcriptionClearedRef.current) return;

    if (event.type === "transcribe" && event.data?.result) {
      const text = event.data.result.trim();
      sliceTextsRef.current.set(event.sliceIndex, text);

      // Build full text from all slices sorted by index
      const parts: string[] = [];
      const sorted = [...sliceTextsRef.current.entries()].sort(
        ([a], [b]) => a - b
      );
      for (const [, t] of sorted) {
        if (t) parts.push(t);
      }
      const fullText = parts.join(" ");

      // If recording was stopped, all remaining text is confirmed
      if (stoppedRef.current) {
        setConfirmedText(fullText);
        setPendingText("");
        prevFullTextRef.current = fullText;
        return;
      }

      // Compare words with previous result to find stable prefix
      const prev = prevFullTextRef.current;
      if (!prev) {
        setConfirmedText("");
        setPendingText(fullText);
      } else {
        const prevWords = prev.split(/\s+/).filter(Boolean);
        const newWords = fullText.split(/\s+/).filter(Boolean);
        let matchCount = 0;
        while (
          matchCount < prevWords.length &&
          matchCount < newWords.length &&
          prevWords[matchCount] === newWords[matchCount]
        ) {
          matchCount++;
        }
        if (matchCount > 0) {
          setConfirmedText(newWords.slice(0, matchCount).join(" "));
          setPendingText(newWords.slice(matchCount).join(" "));
        } else {
          setConfirmedText("");
          setPendingText(fullText);
        }
      }

      prevFullTextRef.current = fullText;

      // Promote pending text to confirmed after 3s of no new transcription
      if (idleTimerRef.current) clearTimeout(idleTimerRef.current);
      idleTimerRef.current = setTimeout(() => {
        idleTimerRef.current = null;
        if (stoppedRef.current || destroyedRef.current) return;
        const current = prevFullTextRef.current;
        if (current) {
          setConfirmedText(current);
          setPendingText("");
        }
      }, 3000);
    }

    if (event.type === "error") {
      if (!destroyedRef.current) {
        const detail = event.data?.result;
        setVoiceError(detail ? `Transcription error: ${detail}` : "Transcription error");
      }
    }
  }, []);

  const onError = useCallback((err: string) => {
    if (destroyedRef.current) return;
    setVoiceError(err);
  }, []);

  const onStatusChange = useCallback((isActive: boolean) => {
    if (destroyedRef.current) return;
    if (!isActive && !stoppedRef.current) {
      // Audio stream stopped unexpectedly — release transcriber to unblock next start
      if (idleTimerRef.current) {
        clearTimeout(idleTimerRef.current);
        idleTimerRef.current = null;
      }
      const transcriber = transcriberRef.current;
      transcriberRef.current = null;
      audioStreamRef.current = null;
      // Timeout-guarded release — if native audio module is in error state
      // (which triggered this unexpected stop), release() may also hang.
      if (transcriber) {
        raceTimeout(transcriber.release(), 5000).catch(() => {});
      }
      setVoiceError("Recording stopped unexpectedly — tap to try again");
      // Promote any partial transcription so the user can still see/use
      // what was already transcribed before the unexpected stop.
      if (prevFullTextRef.current) {
        setConfirmedText(prevFullTextRef.current);
        setPendingText("");
        setRecordingStatus("result");
      } else {
        setRecordingStatus("idle");
      }
    }
  }, []);

  // Check model status on mount
  useEffect(() => {
    destroyedRef.current = false;

    (async () => {
      try {
        const downloaded = isModelDownloaded();
        if (destroyedRef.current) return;

        // On iOS, both ggml model AND CoreML assets must be present to proceed.
        const coremlReady = Platform.OS !== "ios" || isCoreMLDownloaded();

        if (downloaded && coremlReady) {
          updateModelStatus("initializing");
          try {
            await whisperManager.init();
          } catch (firstErr) {
            // Skip retry for timeout errors — if native init hung for 30s,
            // retrying will almost certainly hit the same 30s timeout again
            // (61.5s total wait for corrupt models). Go straight to deletion.
            const isTimeout = firstErr instanceof Error && firstErr.message.includes("timed out");
            if (isTimeout) throw firstErr;
            // Retry once — transient failures (iOS memory pressure, thread
            // crash) shouldn't force a ~1GB re-download. Only delete on
            // second failure, which likely indicates genuine corruption.
            // Brief delay lets the OS reclaim memory before retrying.
            if (destroyedRef.current) return;
            console.warn("[voice] init failed, retrying once:", firstErr);
            await new Promise((r) => setTimeout(r, 1500));
            if (destroyedRef.current) return;
            await whisperManager.init();
          }
          if (destroyedRef.current) return;
          updateModelStatus("ready");
        } else {
          updateModelStatus("not_downloaded");
        }
      } catch (err) {
        if (destroyedRef.current) return;
        // Init failed twice with both files present — likely corrupt.
        // Delete both and prompt re-download.
        try { deleteModel(); } catch {}
        try { deleteCoreML(); } catch {}
        updateModelStatus("not_downloaded");
        setVoiceError(
          err instanceof Error ? err.message : "Failed to initialize model"
        );
      }
    })();

    return () => {
      destroyedRef.current = true;
      stoppedRef.current = true;
      if (idleTimerRef.current) clearTimeout(idleTimerRef.current);
      // Null out refs — do NOT call native stop/release (segfault risk per Pan Winda pattern)
      transcriberRef.current = null;
      audioStreamRef.current = null;
    };
  }, [updateModelStatus]);

  const initModel = useCallback(async () => {
    if (destroyedRef.current) return;
    if (
      modelStatusRef.current === "downloading" ||
      modelStatusRef.current === "initializing" ||
      modelStatusRef.current === "ready"
    ) {
      return;
    }
    setVoiceError(null);
    setDownloadProgress(0);
    setDownloadStage(null);

    // Track current stage so catch can do surgical cleanup.
    let stage: "ggml" | "coreml" | "init" = "ggml";
    try {
      // Stage 1: Download ggml model if needed
      const downloaded = isModelDownloaded();
      if (!downloaded) {
        updateModelStatus("downloading");
        setDownloadStage("model");
        await downloadModel((progress) => {
          if (!destroyedRef.current) {
            setDownloadProgress(progress);
          }
        });
      }

      if (destroyedRef.current) return;

      // Stage 2 (iOS only): Download CoreML assets if needed
      if (Platform.OS === "ios" && !isCoreMLDownloaded()) {
        stage = "coreml";
        updateModelStatus("downloading");
        setDownloadStage("coreml");
        setDownloadProgress(0);
        await downloadCoreML(
          (progress) => {
            if (!destroyedRef.current) {
              setDownloadProgress(progress);
            }
          },
          () => {
            if (!destroyedRef.current) {
              setDownloadStage("extracting");
            }
          },
        );
      }

      if (destroyedRef.current) return;
      stage = "init";
      setDownloadStage(null);
      updateModelStatus("initializing");
      try {
        await whisperManager.init();
      } catch (firstInitErr) {
        // Skip retry for timeout errors — if native init hung for 30s,
        // retrying will almost certainly hit the same 30s timeout again
        // (61.5s total wait for corrupt models). Go straight to deletion.
        const isTimeout = firstInitErr instanceof Error && firstInitErr.message.includes("timed out");
        if (isTimeout) throw firstInitErr;
        // Retry once — transient failures (iOS memory pressure, thread crash)
        // shouldn't force a ~1GB re-download. Matches mount effect retry (R14).
        if (destroyedRef.current) return;
        console.warn("[voice] initModel init failed, retrying once:", firstInitErr);
        await new Promise((r) => setTimeout(r, 1500));
        if (destroyedRef.current) return;
        await whisperManager.init();
      }

      if (destroyedRef.current) return;
      updateModelStatus("ready");
    } catch (err) {
      if (destroyedRef.current) return;
      setDownloadStage(null);
      // Surgical cleanup: only delete assets relevant to the failed stage.
      // downloadModel/downloadCoreML already clean up their own partial files,
      // so we only need to handle the case where a previously-downloaded asset
      // needs removal (e.g., init failure suggests corruption).
      if (stage === "ggml") {
        // downloadModel() already deleted its partial; don't touch CoreML.
      } else if (stage === "coreml") {
        // downloadCoreML() already deleted its partial zip; don't touch GGML
        // which was already verified or successfully downloaded.
      } else {
        // Init failed twice — can't determine which file is corrupt, delete both.
        try { deleteModel(); } catch {}
        try { deleteCoreML(); } catch {}
      }
      updateModelStatus("not_downloaded");
      setVoiceError(
        err instanceof Error ? err.message : "Failed to initialize model"
      );
    }
  }, [updateModelStatus]);

  const startRecording = useCallback(async () => {
    if (destroyedRef.current) return;

    const ctx = whisperManager.getContext();
    if (!ctx) {
      setVoiceError("Voice model not ready — please restart the app");
      return;
    }

    // Guard: don't start if already recording or another start is in progress
    if (transcriberRef.current || isStartingRecordingRef.current) return;
    isStartingRecordingRef.current = true;

    // Mark intent to record — stopRecording checks this to abort
    stoppedRef.current = false;

    // Request mic permission (async — user may release button during this).
    // Wrapped in try/catch: if PermissionsAndroid.request() rejects (e.g., app
    // backgrounded, Activity destroyed), the re-entry guard must be cleared.
    let micResult: string | true;
    try {
      micResult = await requestMicPermission();
    } catch {
      isStartingRecordingRef.current = false;
      setVoiceError("Microphone permission check failed");
      return;
    }
    if (micResult !== true) {
      isStartingRecordingRef.current = false;
      // "never_ask_again": system won't show dialog again — guide to Settings.
      // "denied": user can be re-prompted on next attempt.
      setVoiceError(
        micResult === "never_ask_again"
          ? "Microphone permission required. Check Settings → Apps → Nupi → Permissions."
          : "Microphone permission required"
      );
      return;
    }
    // Check if user released button during permission dialog
    if (stoppedRef.current || destroyedRef.current) {
      isStartingRecordingRef.current = false;
      return;
    }

    try {
      setVoiceError(null);
      transcriptionClearedRef.current = false;
      // Cancel stale idle timer from a previous recording (defense-in-depth —
      // stopRecording and onStatusChange also clear it, but a missed path
      // could leave the timer pending across recording sessions).
      if (idleTimerRef.current) {
        clearTimeout(idleTimerRef.current);
        idleTimerRef.current = null;
      }
      sliceTextsRef.current.clear();
      prevFullTextRef.current = "";
      setConfirmedText("");
      setPendingText("");

      // Fresh audio pipeline per recording session
      const audioStream = new AudioPcmStreamAdapter();
      audioStreamRef.current = audioStream;

      const transcriber = new RealtimeTranscriber(
        {
          whisperContext: ctx,
          audioStream,
        },
        {
          audioSliceSec: 30,
          audioMinSec: 0.5,
          maxSlicesInMemory: 1,
          // language: "auto" = auto-detect spoken language (Whisper supports 99 languages).
          // TODO: Epic for user-configurable language selection in app settings.
          transcribeOptions: { language: "auto", maxThreads: 4 },
        },
        {
          onTranscribe,
          onError,
          onStatusChange,
        }
      );
      transcriberRef.current = transcriber;

      // Timeout-guarded: if native audio module is unresponsive (e.g., iOS
      // audio session conflict), start() could hang forever, leaving
      // isStartingRecordingRef true and blocking all future recordings.
      await raceTimeout(transcriber.start(), 10_000, "Recording start timed out");

      // Check if user released button during start()
      if (stoppedRef.current || destroyedRef.current) {
        // Only clean up if stopRecording hasn't already released this
        // transcriber. stopRecording nulls transcriberRef after release —
        // calling stop/release on an already-released native transcriber
        // risks crashes (same class as WhisperContext segfault concern).
        // Also check isStoppingRecordingRef — if stopRecording is currently
        // in-progress (captured ref but hasn't nulled it yet), skip cleanup
        // to avoid concurrent native stop()/release() on the same transcriber.
        if (transcriberRef.current && !isStoppingRecordingRef.current) {
          transcriberRef.current = null;
          audioStreamRef.current = null;
          // Timeout-guarded — matching stopRecording/onStatusChange patterns.
          // Without timeout, a hung native stop()/release() would leave
          // isStartingRecordingRef true forever, blocking all future recordings.
          try { await raceTimeout(transcriber.stop(), 5000); } catch {}
          try { await raceTimeout(transcriber.release(), 5000); } catch {}
        }
        isStartingRecordingRef.current = false;
        return;
      }
      // Check if onStatusChange(false) already fired during start() —
      // e.g., audio session interrupted immediately. onStatusChange nulls
      // transcriberRef and sets idle/result. Without this guard,
      // setRecordingStatus("recording") overwrites the correction, leaving
      // the UI stuck in recording state with no live transcriber.
      if (!transcriberRef.current) {
        isStartingRecordingRef.current = false;
        return;
      }
      isStartingRecordingRef.current = false;
      setRecordingStatus("recording");
    } catch (err) {
      isStartingRecordingRef.current = false;
      if (destroyedRef.current) return;
      // Release native transcriber resources before dropping refs — without
      // this, a failed start() leaks listeners and native audio state.
      const transcriber = transcriberRef.current;
      if (transcriber) {
        // Timeout-guarded — if native module is in error state (which
        // triggered the catch), release() may also hang.
        try { await raceTimeout(transcriber.release(), 5000); } catch {}
      }
      transcriberRef.current = null;
      audioStreamRef.current = null;
      if (destroyedRef.current) return;
      // On iOS, mic permission denial throws an opaque native error (no
      // PermissionsAndroid equivalent). Append Settings guidance so the user
      // knows how to recover — harmlessly informative for non-permission errors.
      const raw = err instanceof Error ? err.message : "Failed to start recording";
      setVoiceError(
        Platform.OS === "ios"
          ? `${raw}. Check microphone permission in Settings → Nupi.`
          : raw
      );
    }
  }, [onTranscribe, onError, onStatusChange]);

  const stopRecording = useCallback(async () => {
    if (destroyedRef.current) return;

    // Always set stoppedRef — even if transcriber isn't created yet.
    // This aborts an in-progress startRecording that's awaiting permission/start.
    stoppedRef.current = true;

    const transcriber = transcriberRef.current;
    if (!transcriber) return;

    // Re-entry guard: concurrent stopRecording calls (onPressOut + sessionStopped
    // effect + unmount cleanup) must not double-stop the same native transcriber.
    if (isStoppingRecordingRef.current) return;
    isStoppingRecordingRef.current = true;

    try {
      if (idleTimerRef.current) {
        clearTimeout(idleTimerRef.current);
        idleTimerRef.current = null;
      }

      // Timeout guard: native stop() should be near-instant, but if the audio
      // capture module is unresponsive (iOS audio session conflict), waiting
      // indefinitely would freeze the UI with pulsing animation stuck on.
      await raceTimeout(transcriber.stop(), 5000);

      // Release immediately to avoid double-stop on unmount.
      // Also timeout-guarded — if stop() hung and we fell through via timeout,
      // release() on a half-stopped transcriber could also hang.
      try { await raceTimeout(transcriber.release(), 5000); } catch {}
      transcriberRef.current = null;
      audioStreamRef.current = null;
      isStoppingRecordingRef.current = false;

      if (!destroyedRef.current && !transcriptionClearedRef.current) {
        // Promote all text to confirmed
        setConfirmedText(prevFullTextRef.current);
        setPendingText("");
        setRecordingStatus("result");
      }
    } catch (err) {
      if (!destroyedRef.current) {
        setVoiceError(
          err instanceof Error ? err.message : "Failed to stop recording"
        );
        // Promote any accumulated text so the user can still see what was
        // transcribed before the stop failure. Matches onStatusChange behavior
        // for unexpected stops — the text was successfully captured by
        // onTranscribe during recording, only the stop phase failed.
        if (!transcriptionClearedRef.current && prevFullTextRef.current) {
          setConfirmedText(prevFullTextRef.current);
          setPendingText("");
          setRecordingStatus("result");
        } else {
          setRecordingStatus("idle");
        }
      }
      // On error, still try to release to avoid leaked native resources.
      // Timeout-guarded — if stop() crashed the native module, release() may
      // also hang, permanently blocking isStoppingRecordingRef.
      try {
        if (transcriber) { await raceTimeout(transcriber.release(), 5000); }
      } catch {}
      transcriberRef.current = null;
      audioStreamRef.current = null;
      sliceTextsRef.current.clear();
      prevFullTextRef.current = "";
      isStoppingRecordingRef.current = false;
    }
  }, []);

  const clearTranscription = useCallback(() => {
    if (destroyedRef.current) return;
    transcriptionClearedRef.current = true;
    if (idleTimerRef.current) {
      clearTimeout(idleTimerRef.current);
      idleTimerRef.current = null;
    }
    setConfirmedText("");
    setPendingText("");
    sliceTextsRef.current.clear();
    prevFullTextRef.current = "";
    setRecordingStatus("idle");
  }, []);

  const clearVoiceError = useCallback(() => {
    if (destroyedRef.current) return;
    setVoiceError(null);
  }, []);

  // Note: context value changes on every downloadProgress tick and transcription
  // update, re-rendering all consumers (currently only session screen). This is
  // the standard React Context tradeoff — acceptable because the session screen's
  // heavy rendering (WebView) is ref-based and unaffected by React reconciliation.
  // If performance becomes an issue, split into ModelContext + RecordingContext.
  const value = React.useMemo(
    () => ({
      modelStatus,
      recordingStatus,
      confirmedText,
      pendingText,
      voiceError,
      downloadProgress,
      downloadStage,
      initModel,
      startRecording,
      stopRecording,
      clearTranscription,
      clearVoiceError,
    }),
    [
      modelStatus,
      recordingStatus,
      confirmedText,
      pendingText,
      voiceError,
      downloadProgress,
      downloadStage,
      initModel,
      startRecording,
      stopRecording,
      clearTranscription,
      clearVoiceError,
    ]
  );

  return (
    <VoiceContext.Provider value={value}>{children}</VoiceContext.Provider>
  );
}
