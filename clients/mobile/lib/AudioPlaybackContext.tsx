import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import type { AudioControlMessage } from "./audioProtocol";
import {
  useAudioWebSocket,
  type AudioWsStatus,
} from "./useAudioWebSocket";
import {
  useAudioPlayback,
  type PlaybackState,
} from "./useAudioPlayback";
import { getTtsEnabled, addTtsChangeListener } from "./storage";

// Cap buffered PCM frames (~10s at 20ms/frame) to prevent unbounded memory
// growth if audio_meta never arrives (server bug, network corruption).
const MAX_PENDING_PCM_FRAMES = 500;

interface AudioPlaybackContextValue {
  /** Current playback state (idle/playing). */
  playbackState: PlaybackState;
  /** Whether TTS audio is currently playing. */
  isPlaying: boolean;
  /** Stop playback immediately (barge-in). */
  stopPlayback: () => void;
  /** Send interrupt to daemon (barge-in). */
  sendInterrupt: (reason: string) => void;
  /** Whether TTS is enabled (daemon has TTS + user pref on). */
  ttsEnabled: boolean;
  /** Audio WebSocket connection status. */
  audioWsStatus: AudioWsStatus;
  /** Non-null when audio playback engine failed (AudioContext creation error). */
  playbackError: string | null;
}

const AudioPlaybackContext = createContext<AudioPlaybackContextValue>({
  playbackState: "idle",
  isPlaying: false,
  stopPlayback: () => {},
  sendInterrupt: () => {},
  ttsEnabled: false,
  audioWsStatus: "idle",
  playbackError: null,
});

export function useAudioPlaybackContext() {
  return useContext(AudioPlaybackContext);
}

interface AudioPlaybackProviderProps {
  sessionId: string | null;
  children: React.ReactNode;
}

/**
 * Context provider composing useAudioWebSocket + useAudioPlayback hooks.
 * Manages TTS audio lifecycle: connects audio WebSocket, routes binary frames
 * to playback, handles control messages, and exposes barge-in API.
 */
export function AudioPlaybackProvider({
  sessionId,
  children,
}: AudioPlaybackProviderProps) {
  const mountedRef = useRef(true);
  const [playbackState, setPlaybackState] = useState<PlaybackState>("idle");
  const [ttsEnabled, setTtsEnabled] = useState(false);
  // null = not yet loaded from storage; prevents premature WS connect.
  const [userTtsEnabled, setUserTtsEnabled] = useState<boolean | null>(null);
  const ttsCapableRef = useRef(false);
  const capsReceivedRef = useRef(false);
  // Tracks when capabilities check completed with no TTS available.
  // Prevents infinite reconnect loop: connect → no TTS → disconnect("closed") → connect → ...
  const [capsCheckedNoTts, setCapsCheckedNoTts] = useState(false);

  // Buffer binary PCM frames until the first audio_meta text frame arrives.
  // The server sends binary data BEFORE audio_meta (format info), so without
  // buffering the first frame(s) would play at the default sample rate which
  // may differ from the actual TTS format.
  const awaitingFirstMetaRef = useRef(true);
  const pendingPcmBufferRef = useRef<ArrayBuffer[]>([]);
  // Fallback timer — if audio_meta doesn't arrive within 2s of first PCM
  // frame, flush buffer at default format to avoid indefinite silence.
  const pcmBufferTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const handlePlaybackStateChange = useCallback((state: PlaybackState) => {
    if (mountedRef.current) setPlaybackState(state);
  }, []);

  const {
    enqueuePCM,
    stopPlayback: pbStopPlayback,
    updateFormat,
    markDraining,
    playbackError,
  } = useAudioPlayback({ onStateChange: handlePlaybackStateChange });

  // Wrapped stopPlayback that also resets PCM buffering state.
  const stopPlaybackAndReset = useCallback(() => {
    pbStopPlayback();
    awaitingFirstMetaRef.current = true;
    pendingPcmBufferRef.current = [];
    if (pcmBufferTimeoutRef.current) {
      clearTimeout(pcmBufferTimeoutRef.current);
      pcmBufferTimeoutRef.current = null;
    }
  }, [pbStopPlayback]);

  // Load user TTS preference and subscribe to cross-screen changes.
  useEffect(() => {
    mountedRef.current = true;
    (async () => {
      const enabled = await getTtsEnabled();
      if (mountedRef.current) setUserTtsEnabled(enabled);
    })();
    // Subscribe so toggling TTS in Settings takes effect immediately
    // without requiring session screen re-mount.
    const unsubscribe = addTtsChangeListener((enabled) => {
      if (mountedRef.current) {
        setUserTtsEnabled(enabled);
        // Reset so re-enabling TTS retries capability check.
        if (enabled) setCapsCheckedNoTts(false);
      }
    });
    return () => {
      mountedRef.current = false;
      unsubscribe();
      if (pcmBufferTimeoutRef.current) {
        clearTimeout(pcmBufferTimeoutRef.current);
        pcmBufferTimeoutRef.current = null;
      }
    };
  }, []);

  const handlePcmData = useCallback(
    (data: ArrayBuffer) => {
      if (awaitingFirstMetaRef.current) {
        if (pendingPcmBufferRef.current.length >= MAX_PENDING_PCM_FRAMES) {
          // Drop entire buffer instead of O(n) shift() per frame.
          // audio_meta should arrive within a few frames; hitting the cap
          // means the server likely has a bug — discard stale data.
          console.warn("[AudioPlayback] PCM buffer full — audio_meta not received, dropping buffer");
          pendingPcmBufferRef.current = [];
        }
        pendingPcmBufferRef.current.push(data);
        // Start fallback timer on first buffered frame — if audio_meta doesn't
        // arrive within 2s, flush buffer at default format to avoid silence.
        if (!pcmBufferTimeoutRef.current) {
          pcmBufferTimeoutRef.current = setTimeout(() => {
            pcmBufferTimeoutRef.current = null;
            if (awaitingFirstMetaRef.current && mountedRef.current) {
              console.warn("[AudioPlayback] audio_meta timeout — flushing PCM buffer at default format");
              awaitingFirstMetaRef.current = false;
              const pending = pendingPcmBufferRef.current;
              pendingPcmBufferRef.current = [];
              for (const buf of pending) {
                enqueuePCM(buf);
              }
            }
          }, 2000);
        }
        return;
      }
      enqueuePCM(data);
    },
    [enqueuePCM]
  );

  // Ref to audioWs so handleControlMessage can disconnect without dep cycle.
  const audioWsRef = useRef<ReturnType<typeof useAudioWebSocket> | null>(null);

  const handleControlMessage = useCallback(
    (msg: AudioControlMessage) => {
      switch (msg.type) {
        case "audio_meta":
          // Clear PCM buffer fallback timer — format info arrived in time.
          if (pcmBufferTimeoutRef.current) {
            clearTimeout(pcmBufferTimeoutRef.current);
            pcmBufferTimeoutRef.current = null;
          }
          updateFormat(msg.format);
          // Flush buffered PCM frames now that format is known.
          // Server sends binary data BEFORE audio_meta, so the first
          // frame(s) are held until we receive format information.
          if (awaitingFirstMetaRef.current) {
            awaitingFirstMetaRef.current = false;
            // Stop any previous stream remnant (resets drain state).
            pbStopPlayback();
            // If this is the first audio_meta of a NEW stream (msg.first),
            // discard any buffered PCM from a previous interrupted stream.
            // Without this, in-flight frames arriving after barge-in would
            // leak into the next stream's playback.
            if (msg.first) {
              pendingPcmBufferRef.current = [];
            }
            // Flush buffered PCM now that format is known.
            const pending = pendingPcmBufferRef.current;
            pendingPcmBufferRef.current = [];
            for (const buf of pending) {
              enqueuePCM(buf);
            }
          }
          // End-of-stream: let queued buffers play out, then transition
          // to idle when the last buffer finishes (via onEnded callback).
          if (msg.last) {
            markDraining();
            awaitingFirstMetaRef.current = true;
          }
          break;
        case "capabilities_response":
          capsReceivedRef.current = true;
          if (msg.playback.length > 0) {
            ttsCapableRef.current = true;
            if (mountedRef.current) {
              setTtsEnabled(true);
              setCapsCheckedNoTts(false);
            }
          } else {
            ttsCapableRef.current = false;
            if (mountedRef.current) {
              setTtsEnabled(false);
              // Prevent connect effect from re-triggering after disconnect.
              setCapsCheckedNoTts(true);
            }
            // No TTS available — disconnect audio WS (AC5: don't keep WS when TTS not expected).
            audioWsRef.current?.disconnect();
          }
          break;
        case "close_stream":
          // Let remaining queued buffers play out, then transition to idle
          // when the last buffer finishes (detected via onEnded callback).
          markDraining();
          // Reset so next TTS stream buffers PCM until audio_meta arrives
          // with the new format (mirrors the reset in audio_meta.last path).
          awaitingFirstMetaRef.current = true;
          break;
      }
    },
    [updateFormat, markDraining, enqueuePCM, pbStopPlayback]
  );

  const audioWs = useAudioWebSocket(sessionId ?? "", {
    onPcmData: handlePcmData,
    onControlMessage: handleControlMessage,
  });
  const { status: audioWsStatus, connect: audioWsConnect, disconnect: audioWsDisconnect, sendInterrupt: audioWsSendInterrupt } = audioWs;

  // Keep ref in sync via effect (not in render body) to follow React conventions.
  useEffect(() => {
    audioWsRef.current = audioWs;
  });

  // Connect audio WebSocket when session is active and user has TTS enabled.
  // AC5 compliance: connects to query daemon capabilities; disconnects immediately
  // if no TTS playback streams are available (see handleControlMessage).
  // userTtsEnabled === null means preference not yet loaded — don't connect yet.
  // capsCheckedNoTts prevents infinite reconnect loop when daemon has no TTS:
  // connect → caps response (no TTS) → disconnect("closed") → effect re-fires → connect → ...
  useEffect(() => {
    if (sessionId && userTtsEnabled === true && !capsCheckedNoTts && (audioWsStatus === "idle" || audioWsStatus === "closed")) {
      audioWsConnect();
    }
  }, [sessionId, userTtsEnabled, capsCheckedNoTts, audioWsStatus, audioWsConnect]);

  // Auto-recovery from audio WS error state. When all reconnect attempts are
  // exhausted, audioWsStatus becomes "error" and the connect effect above won't
  // fire (it only handles "idle"/"closed"). After a 10s cooldown, retry — this
  // gives transient network issues time to resolve without hammering.
  useEffect(() => {
    if (audioWsStatus === "error" && sessionId && userTtsEnabled === true && !capsCheckedNoTts) {
      const timer = setTimeout(() => {
        if (mountedRef.current) {
          audioWsConnect();
        }
      }, 10_000);
      return () => clearTimeout(timer);
    }
  }, [audioWsStatus, sessionId, userTtsEnabled, capsCheckedNoTts, audioWsConnect]);

  // Capabilities response timeout — disconnect if daemon doesn't respond within 5s.
  // After reconnection with confirmed TTS capability, skip the timeout to avoid
  // premature disconnect on slow responses. The capabilities response will update
  // state when it arrives; if TTS was removed, handleControlMessage disconnects.
  useEffect(() => {
    if (audioWsStatus === "connected") {
      if (ttsCapableRef.current) return;
      capsReceivedRef.current = false;
      const timer = setTimeout(() => {
        if (!capsReceivedRef.current && mountedRef.current) {
          // Daemon didn't respond — treat as no TTS; block reconnect.
          setCapsCheckedNoTts(true);
          audioWsRef.current?.disconnect();
        }
      }, 5000);
      return () => clearTimeout(timer);
    }
  }, [audioWsStatus]);

  // Disconnect audio WS when session is cleared or user explicitly disables TTS.
  // Skip when userTtsEnabled is null (not yet loaded).
  useEffect(() => {
    if (!sessionId || userTtsEnabled === false) {
      if (audioWsStatus !== "idle" && audioWsStatus !== "closed") {
        audioWsDisconnect();
      }
    }
  }, [sessionId, userTtsEnabled, audioWsStatus, audioWsDisconnect]);

  // Stop playback when session is cleared or user disables TTS.
  // Separate effect avoids redundant stopPlaybackAndReset calls on
  // audioWsStatus transitions (e.g., "connected" → "closed" after disconnect).
  useEffect(() => {
    if (!sessionId || userTtsEnabled === false) {
      stopPlaybackAndReset();
    }
  }, [sessionId, userTtsEnabled, stopPlaybackAndReset]);

  const value = useMemo(
    () => ({
      playbackState,
      isPlaying: playbackState === "playing",
      stopPlayback: stopPlaybackAndReset,
      sendInterrupt: audioWsSendInterrupt,
      ttsEnabled: ttsEnabled && userTtsEnabled === true,
      audioWsStatus,
      playbackError,
    }),
    [playbackState, stopPlaybackAndReset, audioWsSendInterrupt, ttsEnabled, userTtsEnabled, audioWsStatus, playbackError]
  );

  return (
    <AudioPlaybackContext.Provider value={value}>
      {children}
    </AudioPlaybackContext.Provider>
  );
}
