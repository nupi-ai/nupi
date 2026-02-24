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
import { getTtsEnabled } from "./storage";

interface AudioPlaybackContextValue {
  /** Current playback state (idle/playing/paused). */
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
}

const AudioPlaybackContext = createContext<AudioPlaybackContextValue>({
  playbackState: "idle",
  isPlaying: false,
  stopPlayback: () => {},
  sendInterrupt: () => {},
  ttsEnabled: false,
  audioWsStatus: "idle",
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
  const [userTtsEnabled, setUserTtsEnabled] = useState(true);
  const ttsCapableRef = useRef(false);

  const playback = useAudioPlayback();

  // Load user TTS preference.
  useEffect(() => {
    mountedRef.current = true;
    (async () => {
      const enabled = await getTtsEnabled();
      if (mountedRef.current) setUserTtsEnabled(enabled);
    })();
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const handlePcmData = useCallback(
    (data: ArrayBuffer) => {
      playback.enqueuePCM(data);
      if (playbackState !== "playing") {
        setPlaybackState("playing");
      }
    },
    [playback, playbackState]
  );

  const handleControlMessage = useCallback(
    (msg: AudioControlMessage) => {
      switch (msg.type) {
        case "audio_meta":
          playback.updateFormat(msg.format);
          break;
        case "capabilities_response":
          if (msg.playback && msg.playback.length > 0) {
            ttsCapableRef.current = true;
            if (mountedRef.current) setTtsEnabled(true);
          } else {
            ttsCapableRef.current = false;
            if (mountedRef.current) setTtsEnabled(false);
          }
          break;
        case "close_stream":
          // Let remaining queued buffers play out, then transition to idle.
          // AudioBufferQueueSourceNode drains automatically.
          // We set a timeout to check if playback finished.
          setTimeout(() => {
            if (mountedRef.current) {
              setPlaybackState("idle");
            }
          }, 500);
          break;
      }
    },
    [playback]
  );

  const audioWs = useAudioWebSocket(sessionId ?? "", {
    onPcmData: handlePcmData,
    onControlMessage: handleControlMessage,
  });

  // Connect audio WebSocket when session is active and user has TTS enabled.
  useEffect(() => {
    if (sessionId && userTtsEnabled && audioWs.status === "idle") {
      audioWs.connect();
    }
  }, [sessionId, userTtsEnabled, audioWs]);

  // Disconnect when session is cleared or user disables TTS.
  useEffect(() => {
    if (!sessionId || !userTtsEnabled) {
      if (audioWs.status !== "idle" && audioWs.status !== "closed") {
        audioWs.disconnect();
      }
      playback.stopPlayback();
      setPlaybackState("idle");
    }
  }, [sessionId, userTtsEnabled, audioWs, playback]);

  const stopPlayback = useCallback(() => {
    playback.stopPlayback();
    setPlaybackState("idle");
  }, [playback]);

  const sendInterrupt = useCallback(
    (reason: string) => {
      audioWs.sendInterrupt(reason);
    },
    [audioWs]
  );

  const value = useMemo(
    () => ({
      playbackState,
      isPlaying: playbackState === "playing",
      stopPlayback,
      sendInterrupt,
      ttsEnabled: ttsEnabled && userTtsEnabled,
      audioWsStatus: audioWs.status,
    }),
    [playbackState, stopPlayback, sendInterrupt, ttsEnabled, userTtsEnabled, audioWs.status]
  );

  return (
    <AudioPlaybackContext.Provider value={value}>
      {children}
    </AudioPlaybackContext.Provider>
  );
}
