import { useCallback, useEffect, useRef } from "react";

import { Platform } from "react-native";
import {
  AudioContext,
  AudioManager,
  type AudioBufferQueueSourceNode,
} from "react-native-audio-api";

import type { AudioFormat } from "./audioProtocol";
import { pcmS16leToFloat32 } from "./audioProtocol";
import { raceTimeout } from "./raceTimeout";

export type PlaybackState = "idle" | "playing" | "paused";

interface UseAudioPlaybackResult {
  /** Current playback state. */
  playbackState: PlaybackState;
  /** Enqueue a PCM s16le ArrayBuffer for playback. Starts playback if idle. */
  enqueuePCM: (pcmData: ArrayBuffer) => void;
  /** Immediately stop playback and clear buffers (barge-in). */
  stopPlayback: () => void;
  /** Resume playback after pause. */
  resumePlayback: () => void;
  /** Update audio format (recreates AudioContext if sample rate changed). */
  updateFormat: (format: AudioFormat) => void;
}

/**
 * React hook managing PCM audio playback via react-native-audio-api's
 * AudioBufferQueueSourceNode. Designed for streaming TTS playback where
 * chunks arrive progressively over WebSocket.
 */
export function useAudioPlayback(): UseAudioPlaybackResult {
  const audioContextRef = useRef<AudioContext | null>(null);
  const queueSourceRef = useRef<AudioBufferQueueSourceNode | null>(null);
  const playbackStateRef = useRef<PlaybackState>("idle");
  const sampleRateRef = useRef(16000);
  const channelsRef = useRef(1);
  const mountedRef = useRef(true);
  const isCreatingRef = useRef(false);
  // Force re-render when playback state changes.
  const forceUpdateRef = useRef(0);
  const [, setForceUpdate] = [forceUpdateRef.current, (v: number) => { forceUpdateRef.current = v; }];

  // Use a state variable for playbackState to trigger re-renders.
  const playbackStateCallbackRef = useRef<PlaybackState>("idle");

  // We use a ref + callback pattern to avoid re-render on every PCM chunk.
  // The context consumers read playbackState from the ref via a getter.
  const stateListenersRef = useRef<Set<() => void>>(new Set());

  const notifyStateChange = useCallback((state: PlaybackState) => {
    playbackStateRef.current = state;
    playbackStateCallbackRef.current = state;
    for (const listener of stateListenersRef.current) {
      listener();
    }
  }, []);

  const ensureAudioContext = useCallback(async (): Promise<{
    ctx: AudioContext;
    queue: AudioBufferQueueSourceNode;
  } | null> => {
    if (audioContextRef.current && queueSourceRef.current) {
      return { ctx: audioContextRef.current, queue: queueSourceRef.current };
    }

    if (isCreatingRef.current) return null;
    isCreatingRef.current = true;

    try {
      // Configure iOS audio session for playback: plays alongside other audio,
      // respects ringer/silent mode, volume controllable via hardware buttons.
      if (Platform.OS === "ios") {
        AudioManager.setAudioSessionOptions({
          iosCategory: "playback",
          iosMode: "default",
          iosOptions: ["mixWithOthers"],
        });
      }

      let ctx = audioContextRef.current;
      if (!ctx) {
        ctx = await raceTimeout(
          Promise.resolve(new AudioContext({ sampleRate: sampleRateRef.current })),
          5000,
          "AudioContext creation timed out",
        );
        if (!mountedRef.current) {
          ctx.close();
          isCreatingRef.current = false;
          return null;
        }
        audioContextRef.current = ctx;
      }

      // Create a fresh queue source (needed after stop() which makes the
      // previous source unusable per Web Audio spec).
      const queue = ctx.createBufferQueueSource();
      queue.connect(ctx.destination);
      queue.start(ctx.currentTime);
      queueSourceRef.current = queue;

      isCreatingRef.current = false;
      return { ctx, queue };
    } catch {
      isCreatingRef.current = false;
      return null;
    }
  }, []);

  const enqueuePCM = useCallback((pcmData: ArrayBuffer) => {
    if (!mountedRef.current) return;

    const float32 = pcmS16leToFloat32(pcmData);
    if (float32.length === 0) return;

    // Ensure AudioContext exists (lazy creation on first chunk).
    ensureAudioContext().then((resources) => {
      if (!resources || !mountedRef.current) return;
      const { ctx, queue } = resources;

      const buffer = ctx.createBuffer(
        channelsRef.current,
        float32.length,
        sampleRateRef.current,
      );
      buffer.copyToChannel(float32, 0);
      queue.enqueueBuffer(buffer);

      if (playbackStateRef.current !== "playing") {
        notifyStateChange("playing");
      }
    });
  }, [ensureAudioContext, notifyStateChange]);

  const stopPlayback = useCallback(() => {
    const queue = queueSourceRef.current;
    const ctx = audioContextRef.current;
    if (queue) {
      // stop() halts the currently playing chunk immediately;
      // clearBuffers() removes any queued-but-unplayed buffers.
      queue.stop(ctx?.currentTime ?? 0);
      queue.clearBuffers();
      // Re-arm the source so future enqueueBuffer calls work.
      queueSourceRef.current = null;
    }
    notifyStateChange("idle");
  }, [notifyStateChange]);

  const resumePlayback = useCallback(() => {
    const queue = queueSourceRef.current;
    if (queue && playbackStateRef.current === "paused") {
      queue.start(audioContextRef.current?.currentTime ?? 0);
      notifyStateChange("playing");
    }
  }, [notifyStateChange]);

  const updateFormat = useCallback((format: AudioFormat) => {
    const newSampleRate = format.sample_rate;
    const newChannels = format.channels;
    channelsRef.current = newChannels;

    if (newSampleRate !== sampleRateRef.current && audioContextRef.current) {
      // Sample rate changed — must recreate AudioContext.
      const oldQueue = queueSourceRef.current;
      if (oldQueue) {
        oldQueue.clearBuffers();
      }
      audioContextRef.current.close();
      audioContextRef.current = null;
      queueSourceRef.current = null;
      sampleRateRef.current = newSampleRate;
      notifyStateChange("idle");
    } else {
      sampleRateRef.current = newSampleRate;
    }
  }, [notifyStateChange]);

  // Cleanup on unmount.
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      const queue = queueSourceRef.current;
      if (queue) {
        queue.clearBuffers();
      }
      queueSourceRef.current = null;
      const ctx = audioContextRef.current;
      if (ctx) {
        ctx.close();
      }
      audioContextRef.current = null;
    };
  }, []);

  return {
    get playbackState() {
      return playbackStateCallbackRef.current;
    },
    enqueuePCM,
    stopPlayback,
    resumePlayback,
    updateFormat,
  };
}
