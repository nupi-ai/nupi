import type { Session } from './session';

interface BaseMessage {
  timestamp: string;
}

export interface SessionsListMessage extends BaseMessage {
  type: 'sessions_list';
  data: Session[];
}

export interface SessionCreatedMessage extends BaseMessage {
  type: 'session_created';
  data: Session;
}

export interface SessionKilledMessage extends BaseMessage {
  type: 'session_killed';
  sessionId: string;
}

export interface SessionStatusChangedMessage extends BaseMessage {
  type: 'session_status_changed';
  sessionId: string;
  data: string;
}

export interface SessionModeChangedMessage extends BaseMessage {
  type: 'session_mode_changed';
  sessionId: string;
  data: { mode: string };
}

export interface ToolDetectedMessage extends BaseMessage {
  type: 'tool_detected';
  sessionId: string;
  data: { tool?: string; tool_icon?: string; tool_icon_data?: string };
}

export interface ResizeInstructionMessage extends BaseMessage {
  type: 'resize_instruction';
  sessionId: string;
  data: { instructions: any[] };
}

export interface AttachedMessage extends BaseMessage {
  type: 'attached';
  sessionId: string;
}

export interface DetachedMessage extends BaseMessage {
  type: 'detached';
  sessionId: string;
}

export interface ErrorMessage extends BaseMessage {
  type: 'error';
  data: string;
}

export type ServerMessage =
  | SessionsListMessage
  | SessionCreatedMessage
  | SessionKilledMessage
  | SessionStatusChangedMessage
  | SessionModeChangedMessage
  | ToolDetectedMessage
  | ResizeInstructionMessage
  | AttachedMessage
  | DetachedMessage
  | ErrorMessage;

const VALID_TYPES = new Set<ServerMessage['type']>([
  'sessions_list',
  'session_created',
  'session_killed',
  'session_status_changed',
  'session_mode_changed',
  'tool_detected',
  'resize_instruction',
  'attached',
  'detached',
  'error',
]);

export function parseServerMessage(raw: string): ServerMessage | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    console.error('[WS] Failed to parse message:', raw);
    return null;
  }

  if (typeof parsed !== 'object' || parsed === null || !('type' in parsed)) {
    console.warn('[WS] Message missing type field:', parsed);
    return null;
  }

  const msg = parsed as { type: string };
  if (!VALID_TYPES.has(msg.type as ServerMessage['type'])) {
    console.warn('[WS] Unknown message type:', msg.type);
    return null;
  }

  return parsed as ServerMessage;
}
