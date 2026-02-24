import { invoke } from '@tauri-apps/api/core';
import type { ErrorCodeValue } from '@nupi/shared/errors';
import type { Session } from '../types/session';
import type { LanguageInfo, Recording } from '../types/protoDtos';

type InvokeArgs = object | undefined;

export type JsonValue = unknown;
export type { LanguageInfo, Recording } from '../types/protoDtos';

export interface CommandErrorPayload {
  code: ErrorCodeValue | string;
  message: string;
  details?: unknown;
}

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

function asCommandErrorPayload(value: unknown): CommandErrorPayload | null {
  if (!value || typeof value !== 'object') {
    return null;
  }

  const candidate = value as Record<string, unknown>;
  if (typeof candidate.code !== 'string' || typeof candidate.message !== 'string') {
    return null;
  }

  return {
    code: candidate.code,
    message: candidate.message,
    details: candidate.details,
  };
}

function extractCommandError(error: unknown): CommandErrorPayload | null {
  const direct = asCommandErrorPayload(error);
  if (direct) {
    return direct;
  }

  if (error && typeof error === 'object') {
    const nested = asCommandErrorPayload((error as Record<string, unknown>).error);
    if (nested) {
      return nested;
    }
  }

  return null;
}

export function toErrorMessage(error: unknown): string {
  const payload = extractCommandError(error);
  if (payload) {
    return payload.message;
  }
  if (error instanceof Error) {
    return error.message;
  }
  return String(error);
}

export function toErrorCode(error: unknown): string | null {
  return extractCommandError(error)?.code ?? null;
}

async function ipc<T>(command: string, args?: InvokeArgs): Promise<T> {
  return invoke<T>(command, args as Record<string, unknown> | undefined);
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
