/**
 * Shared test helpers for Story 11-5 integration tests.
 *
 * Extracts duplicated utilities: QueryClient wrappers, mock NupiClient
 * factories, and ConversationTurn builders used across e2eVoiceLoop,
 * bargeIn, lanOnlyOperation, and multiClientConversation test suites.
 */
import { vi } from "vitest";
import React from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { NupiClient } from "../connect";
import type { ConversationTurn } from "../conversationTypes";
import type { AudioControlMessage } from "../audioProtocol";

/**
 * Creates a QueryClientProvider wrapper for renderHook tests.
 * Each call creates a fresh QueryClient instance to ensure test isolation.
 */
export function createQueryWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return React.createElement(QueryClientProvider, { client: queryClient }, children);
  };
}

/**
 * Creates a mock NupiClient with configurable RPC stubs.
 *
 * @param getConversation - Mock for sessions.getConversation RPC
 * @param sendVoiceCommand - Optional mock for sessions.sendVoiceCommand RPC
 */
export function createMockClient(
  getConversation: (...args: any[]) => Promise<any>,
  sendVoiceCommand?: (...args: any[]) => Promise<any>,
): NupiClient {
  return {
    sessions: {
      getConversation,
      getGlobalConversation: vi.fn(async () => ({ turns: [], total: 0 })),
      sendVoiceCommand: sendVoiceCommand ?? vi.fn(async () => ({ accepted: true, message: "queued" })),
    },
  } as unknown as NupiClient;
}

// ---------------------------------------------------------------------------
// Audio subsystem mock functions (shared between e2eVoiceLoop and bargeIn)
// ---------------------------------------------------------------------------

export const mockConnect = vi.fn();
export const mockDisconnect = vi.fn();
export const mockSendInterrupt = vi.fn();
export const mockEnqueuePCM = vi.fn();
export const mockStopPlayback = vi.fn();
export const mockUpdateFormat = vi.fn();
export const mockMarkDraining = vi.fn();

/**
 * Configures useAudioWebSocket and useAudioPlayback mocks with captured callbacks.
 * Caller must have vi.mock'd both modules and pass the imported references.
 */
export function setupAudioMocks(
  hooks: { useAudioWebSocket: any; useAudioPlayback: any },
  overrides?: { wsStatus?: string; playbackState?: string },
) {
  let capturedOnPcmData: ((data: ArrayBuffer) => void) | undefined;
  let capturedOnControlMessage: ((msg: AudioControlMessage) => void) | undefined;

  vi.mocked(hooks.useAudioWebSocket).mockImplementation((_sid: string, opts: any) => {
    capturedOnPcmData = opts.onPcmData;
    capturedOnControlMessage = opts.onControlMessage;
    return {
      status: (overrides?.wsStatus ?? "idle") as any,
      error: null,
      sendInterrupt: mockSendInterrupt,
      disconnect: mockDisconnect,
      connect: mockConnect,
    };
  });

  vi.mocked(hooks.useAudioPlayback).mockImplementation(() => ({
    playbackState: (overrides?.playbackState ?? "idle") as any,
    playbackError: null,
    enqueuePCM: mockEnqueuePCM,
    stopPlayback: mockStopPlayback,
    updateFormat: mockUpdateFormat,
    markDraining: mockMarkDraining,
  }));

  return {
    /** Returns the captured onPcmData callback. Throws if not yet captured (component not rendered). */
    capturedOnPcmData: (): ((data: ArrayBuffer) => void) => {
      if (!capturedOnPcmData) throw new Error("onPcmData callback not captured — ensure AudioPlaybackProvider was rendered and hook invoked");
      return capturedOnPcmData;
    },
    /** Returns the captured onControlMessage callback. Throws if not yet captured (component not rendered). */
    capturedOnControlMessage: (): ((msg: AudioControlMessage) => void) => {
      if (!capturedOnControlMessage) throw new Error("onControlMessage callback not captured — ensure AudioPlaybackProvider was rendered and hook invoked");
      return capturedOnControlMessage;
    },
  };
}

/** Creates a capabilities_response AudioControlMessage. */
export function capsResponse(streams: string[]): AudioControlMessage {
  return {
    type: "capabilities_response",
    capture: [],
    playback: streams.map((s) => ({ stream_id: s, ready: true })),
  } as AudioControlMessage;
}

/** Creates an audio_meta AudioControlMessage with configurable first/last flags. */
export function audioMeta(overrides?: { last?: boolean; first?: boolean }): AudioControlMessage {
  return {
    type: "audio_meta",
    format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 },
    sequence: 1,
    first: overrides?.first ?? false,
    last: overrides?.last ?? false,
    duration_ms: 20,
  } as AudioControlMessage;
}

/**
 * Creates ConversationTurn objects from simple definitions.
 *
 * Returns turns in **post-transform format** (metadata as flat `Record<string, string>`),
 * matching the output of `useConversationQuery.mapProtoTurn()`. Use this when testing
 * components that consume already-transformed turns (e.g., ConversationPanel).
 *
 * **Date caveat:** Turns use deterministic past timestamps (Jan 1 2024, 00:00:00+i seconds)
 * so they will NEVER exercise ConversationPanel's "today" formatting branch (HH:MM without
 * dd/mm prefix). Tests that need "today" display must construct turns with the current date
 * directly — see the "renders 'today' time format" test in e2eVoiceLoop.test.tsx for an example.
 *
 * For tests that mock the raw proto RPC response (e.g., multiClientConversation),
 * use the proto format: `metadata: [{ key: "input_source", value: "voice" }]`.
 * The hook's `mapProtoTurn()` converts proto arrays to flat objects automatically.
 */
export function makeTurns(...defs: Array<{ origin: string; text: string }>): ConversationTurn[] {
  return defs.map((d, i) => ({
    origin: d.origin as ConversationTurn["origin"],
    text: d.text,
    at: new Date(2024, 0, 1, 0, 0, i),
    metadata: {},
  }));
}

// ---------------------------------------------------------------------------
// Audio context test consumer factory (shared between e2eVoiceLoop and bargeIn)
// ---------------------------------------------------------------------------

/**
 * Factory that creates a test component extracting values from a React context hook.
 * Avoids importing AudioPlaybackContext directly in testHelpers (which would trigger
 * module loading outside vi.mock scope of consuming test files).
 *
 * NOTE: The useEffect has NO dependency array intentionally — it fires after every
 * render so `onValues` always receives the latest context snapshot. This ensures
 * tests that call `values.stopPlayback()` etc. operate on the most recent callbacks
 * even after re-renders triggered by state changes inside the provider.
 */
export function createTestContextConsumer(useContextHook: () => any) {
  return function TestContextConsumer({ onValues }: { onValues: (val: any) => void }) {
    const ctx = useContextHook();
    React.useEffect(() => {
      onValues(ctx);
    });
    return null;
  };
}

// ---------------------------------------------------------------------------
// Auth client mock factory (for notification tests)
// ---------------------------------------------------------------------------

/**
 * Creates a mock NupiClient with auth service stubs for push notification tests.
 *
 * @param overrides - Optional overrides for auth RPC stubs
 */
export function createMockAuthClient(overrides?: {
  registerPushToken?: (...args: any[]) => Promise<any>;
}) {
  return {
    auth: {
      registerPushToken: overrides?.registerPushToken ?? vi.fn(async () => ({})),
      unregisterPushToken: vi.fn(async () => ({})),
    },
  } as unknown as NupiClient;
}
