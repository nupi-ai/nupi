import { isJsonRecord, parseStringRecordLenient } from '@nupi/shared/json';

export const SESSION_EVENT_TYPES = [
  'session_created',
  'session_killed',
  'session_status_changed',
  'session_mode_changed',
  'tool_detected',
  'resize_instruction',
] as const;

export type SessionEventType = (typeof SESSION_EVENT_TYPES)[number];

const SESSION_EVENT_TYPE_SET: ReadonlySet<string> = new Set(SESSION_EVENT_TYPES);

export interface SessionEventPayload {
  event_type: SessionEventType;
  session_id: string;
  data: Record<string, string>;
}

function parseEventType(value: unknown): SessionEventType | null {
  if (typeof value !== 'string') {
    return null;
  }
  return SESSION_EVENT_TYPE_SET.has(value) ? (value as SessionEventType) : null;
}

export function parseSessionEventPayload(raw: unknown): SessionEventPayload | null {
  if (!isJsonRecord(raw)) {
    return null;
  }

  if (typeof raw.session_id !== 'string' || raw.session_id.length === 0) {
    return null;
  }

  const event_type = parseEventType(raw.event_type);
  if (event_type === null) {
    return null;
  }

  return {
    event_type,
    session_id: raw.session_id,
    data: parseStringRecordLenient(raw.data),
  };
}
