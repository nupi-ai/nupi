import { useCallback, useEffect, useRef, useState } from "react";

import { Platform } from "react-native";
import {
  AudioContext,
  AudioManager,
  type AudioBufferQueueSourceNode,
} from "react-native-audio-api";

import type { AudioFormat } from "./audioProtocol";
import { pcmS16leToFloat32 } from "./audioProtocol";

export type PlaybackState = "idle" | "playing";

interface UseAudioPlaybackResult {
  /** Current playback state. */
  playbackState: PlaybackState;
  /** Non-null when AudioContext creation or audio session setup failed. */
  playbackError: string | null;
  /** Enqueue a PCM s16le ArrayBuffer for playback. Starts playback if idle. */
  enqueuePCM: (pcmData: ArrayBuffer) => void;
  /** Immediately stop playback and clear buffers (barge-in). */
  stopPlayback: () => void;
  /** Update audio format (recreates AudioContext if sample rate changed). */
  updateFormat: (format: AudioFormat) => void;
  /** Mark that no more buffers will arrive; transition to idle when queue drains. */
  markDraining: () => void;
}

interface UseAudioPlaybackOptions {
  /** Called whenever playback state changes (for external state sync). */
  onStateChange?: (state: PlaybackState) => void;
}

/**
 * React hook managing PCM audio playback via react-native-audio-api's
 * AudioBufferQueueSourceNode. Designed for streaming TTS playback where
 * chunks arrive progressively over WebSocket.
 */
export function useAudioPlayback(options: UseAudioPlaybackOptions = {}): UseAudioPlaybackResult {
  const onStateChangeRef = useRef(options.onStateChange);
  onStateChangeRef.current = options.onStateChange;
  const audioContextRef = useRef<AudioContext | null>(null);
  const queueSourceRef = useRef<AudioBufferQueueSourceNode | null>(null);
  const playbackStateRef = useRef<PlaybackState>("idle");
  const sampleRateRef = useRef(16000);
  const channelsRef = useRef(1);
  const mountedRef = useRef(true);
  // iOS audio session options are process-level — only need to be set once.
  const audioSessionConfiguredRef = useRef(false);
  // When true, close_stream arrived and we're waiting for queue to drain.
  const drainingRef = useRef(false);
  // Track pending buffer count for accurate drain detection.
  const pendingBufferCountRef = useRef(0);
  // Prevents repeated AudioContext creation attempts after first failure.
  // Reset on format change (updateFormat) or unmount/remount.
  const contextCreationFailedRef = useRef(false);

  // State variable so consumers that read playbackState directly get re-renders.
  const [externalPlaybackState, setExternalPlaybackState] = useState<PlaybackState>("idle");
  const [playbackError, setPlaybackError] = useState<string | null>(null);

  const notifyStateChange = useCallback((state: PlaybackState) => {
    playbackStateRef.current = state;
    setExternalPlaybackState(state);
    onStateChangeRef.current?.(state);
  }, []);

  const ensureAudioContext = useCallback((): {
    ctx: AudioContext;
    queue: AudioBufferQueueSourceNode;
  } | null => {
    if (audioContextRef.current && queueSourceRef.current) {
      return { ctx: audioContextRef.current, queue: queueSourceRef.current };
    }

    // Don't retry after first failure — prevents hammering AudioContext
    // creation N times when flushing buffered PCM frames (up to 500 calls).
    if (contextCreationFailedRef.current) return null;

    try {
      // Configure audio session for TTS playback (once per app lifecycle).
      // iOS: "ambient" category respects ringer/silent switch (AC4) and
      // naturally mixes with other audio. Volume follows system volume.
      // Android: react-native-audio-api v0.11 does not expose Android audio
      // session options — AudioContext defaults are acceptable (media usage,
      // speaker output, system volume).
      if (Platform.OS === "ios" && !audioSessionConfiguredRef.current) {
        try {
          AudioManager.setAudioSessionOptions({
            iosCategory: "ambient",
            iosMode: "default",
          });
          audioSessionConfiguredRef.current = true;
        } catch (e) {
          console.warn("[useAudioPlayback] iOS audio session config failed:", e);
          // Continue — playback may still work with default settings.
        }
      }

      let ctx = audioContextRef.current;
      if (!ctx) {
        // AudioContext constructor is synchronous — no timeout needed.
        ctx = new AudioContext({ sampleRate: sampleRateRef.current });
        if (!mountedRef.current) {
          ctx.close();
          return null;
        }
        audioContextRef.current = ctx;
      }

      // Create a fresh queue source (needed after stop() which makes the
      // previous source unusable per Web Audio spec).
      const queue = ctx.createBufferQueueSource();
      queue.connect(ctx.destination);

      // Detect when queued buffers finish playing (for end-of-stream drain).
      queue.onEnded = (event: { bufferId?: string }) => {
        if (!mountedRef.current) return;
        // bufferId === undefined means the source itself stopped (not relevant here).
        if (event.bufferId !== undefined) {
          pendingBufferCountRef.current = Math.max(0, pendingBufferCountRef.current - 1);
          // Only transition to idle when draining AND all enqueued buffers have played.
          if (drainingRef.current && pendingBufferCountRef.current === 0) {
            drainingRef.current = false;
            notifyStateChange("idle");
          }
        }
      };

      queue.start(ctx.currentTime);
      queueSourceRef.current = queue;

      setPlaybackError(null);
      return { ctx, queue };
    } catch (e) {
      contextCreationFailedRef.current = true;
      setPlaybackError(
        e instanceof Error ? `Audio init failed: ${e.message}` : "Audio init failed",
      );
      return null;
    }
  }, [notifyStateChange]);

  const enqueuePCM = useCallback((pcmData: ArrayBuffer) => {
    if (!mountedRef.current) return;

    const float32 = pcmS16leToFloat32(pcmData);
    if (float32.length === 0) return;

    // Ensure AudioContext exists (lazy creation on first chunk).
    const resources = ensureAudioContext();
    if (!resources || !mountedRef.current) return;
    const { ctx, queue } = resources;

    try {
      const buffer = ctx.createBuffer(
        channelsRef.current,
        float32.length,
        sampleRateRef.current,
      );
      buffer.copyToChannel(float32, 0);
      queue.enqueueBuffer(buffer);
      // Increment AFTER successful enqueue — if enqueueBuffer throws, the
      // counter stays correct and drain detection (pendingBufferCount === 0)
      // won't be permanently broken.
      pendingBufferCountRef.current++;

      if (playbackStateRef.current !== "playing") {
        notifyStateChange("playing");
      }
    } catch {
      // AudioContext may have been closed between check and use.
    }
  }, [ensureAudioContext, notifyStateChange]);

  const stopPlayback = useCallback(() => {
    const queue = queueSourceRef.current;
    const ctx = audioContextRef.current;
    drainingRef.current = false;
    pendingBufferCountRef.current = 0;
    if (queue) {
      // stop() halts the currently playing chunk immediately;
      // clearBuffers() removes any queued-but-unplayed buffers.
      queue.stop(ctx?.currentTime ?? 0);
      queue.clearBuffers();
      // Disconnect from destination and null the ref; ensureAudioContext() will
      // create a fresh queue source on the next enqueuePCM call (stopped
      // sources are not reusable per Web Audio spec).
      queue.disconnect();
      queueSourceRef.current = null;
    }
    notifyStateChange("idle");
  }, [notifyStateChange]);

  const markDraining = useCallback(() => {
    drainingRef.current = true;
    // If there's no queue source, we're already idle, or all buffers already played — transition immediately.
    if (!queueSourceRef.current || playbackStateRef.current === "idle" || pendingBufferCountRef.current === 0) {
      drainingRef.current = false;
      notifyStateChange("idle");
    }
    // Otherwise, onEnded handler will transition to idle when last buffer finishes.
  }, [notifyStateChange]);

  const updateFormat = useCallback((format: AudioFormat) => {
    const newSampleRate = format.sample_rate;
    const newChannels = format.channels;
    channelsRef.current = newChannels;
    // Allow retry after format change — new sample rate may succeed where old one failed.
    contextCreationFailedRef.current = false;

    if (newSampleRate !== sampleRateRef.current && audioContextRef.current) {
      // Sample rate changed — must recreate AudioContext.
      const oldQueue = queueSourceRef.current;
      if (oldQueue) {
        oldQueue.stop(audioContextRef.current?.currentTime ?? 0);
        oldQueue.clearBuffers();
        oldQueue.disconnect();
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
      const ctx = audioContextRef.current;
      if (queue) {
        queue.stop(ctx?.currentTime ?? 0);
        queue.clearBuffers();
        queue.disconnect();
      }
      queueSourceRef.current = null;
      if (ctx) {
        ctx.close();
      }
      audioContextRef.current = null;
    };
  }, []);

  return {
    playbackState: externalPlaybackState,
    playbackError,
    enqueuePCM,
    stopPlayback,
    updateFormat,
    markDraining,
  };
}
