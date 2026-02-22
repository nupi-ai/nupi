import { invoke } from '@tauri-apps/api/core';
import type { Session } from '../types/session';
import type { LanguageInfo, Recording } from '../types/protoDtos';

type InvokeArgs = object | undefined;

export type JsonValue = unknown;
export type { LanguageInfo, Recording } from '../types/protoDtos';

export interface VoiceStreamFromFileRequest {
  sessionId: string;
  streamId: string | null;
  inputPath: string;
  playbackOutput: string | null;
  disablePlayback: boolean;
  metadata: Record<string, string>;
  operationId: string;
}

export interface VoiceInterruptRequest {
  sessionId: string;
  streamId: string | null;
  metadata: Record<string, string>;
}

function normalizeInvokeError(error: unknown): Error {
  if (error instanceof Error) {
    return error;
  }

  return new Error(String(error));
}

export function toErrorMessage(error: unknown): string {
  return normalizeInvokeError(error).message;
}

async function ipc<T>(command: string, args?: InvokeArgs): Promise<T> {
  try {
    return await invoke<T>(command, args as Record<string, unknown> | undefined);
  } catch (error) {
    throw normalizeInvokeError(error);
  }
}

export const api = {
  daemon: {
    status: () => ipc<boolean>('daemon_status'),
  },

  sessions: {
    list: () => ipc<Session[]>('list_sessions'),
    get: (sessionId: string) => ipc<Session>('get_session', { sessionId }),
    kill: (sessionId: string) => ipc<void>('kill_session', { sessionId }),
    attach: (sessionId: string) => ipc<void>('attach_session', { sessionId }),
    detach: (sessionId: string) => ipc<void>('detach_session', { sessionId }),
    sendInput: (sessionId: string, input: number[]) => ipc<void>('send_input', { sessionId, input }),
    resize: (sessionId: string, cols: number, rows: number) =>
      ipc<void>('resize_session', { sessionId, cols, rows }),
  },

  recordings: {
    list: () => ipc<Recording[]>('list_recordings', {}),
    get: (sessionId: string) => ipc<string>('get_recording', { sessionId }),
  },

  voice: {
    streamFromFile: (payload: VoiceStreamFromFileRequest) =>
      ipc<JsonValue>('voice_stream_from_file', payload),
    interrupt: (payload: VoiceInterruptRequest) => ipc<JsonValue>('voice_interrupt_command', payload),
    cancel: (operationId: string) => ipc<boolean>('voice_cancel_stream', { operationId }),
    status: (sessionId: string | null) => ipc<JsonValue>('voice_status_command', { sessionId }),
  },

  language: {
    get: () => ipc<string | null>('get_language'),
    list: () => ipc<LanguageInfo[]>('list_languages'),
    set: (language: string | null) => ipc<void>('set_language', { language }),
  },
};
