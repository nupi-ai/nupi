type JsonRecord = Record<string, unknown>;

const WS_EVENT_MESSAGE_TYPE = "event" as const;

// WebSocket session stream emits eventbus session states, not gRPC SessionEventType.
export const SESSION_STREAM_EVENT_TYPES = [
  "created",
  "running",
  "detached",
  "stopped",
  "resize_instruction",
] as const;

export type SessionStreamEventType = (typeof SESSION_STREAM_EVENT_TYPES)[number];

const SESSION_STREAM_EVENT_TYPE_SET: ReadonlySet<string> = new Set(SESSION_STREAM_EVENT_TYPES);

interface SessionStreamEventBase {
  type: typeof WS_EVENT_MESSAGE_TYPE;
  event_type: SessionStreamEventType;
  session_id: string;
}

export interface SessionCreatedEvent extends SessionStreamEventBase {
  event_type: "created";
  data?: Record<string, string>;
}

export interface SessionRunningEvent extends SessionStreamEventBase {
  event_type: "running";
  data?: Record<string, string>;
}

export interface SessionDetachedEvent extends SessionStreamEventBase {
  event_type: "detached";
  data?: Record<string, string>;
}

export interface SessionStoppedEvent extends SessionStreamEventBase {
  event_type: "stopped";
  data?: Record<string, string>;
}

export interface SessionResizeInstructionEvent extends SessionStreamEventBase {
  event_type: "resize_instruction";
  data?: Record<string, string>;
}

export type SessionStreamEvent =
  | SessionCreatedEvent
  | SessionRunningEvent
  | SessionDetachedEvent
  | SessionStoppedEvent
  | SessionResizeInstructionEvent;

function isJsonRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseStringRecord(value: unknown): Record<string, string> | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (!isJsonRecord(value)) {
    throw new Error("event.data must be an object");
  }
  const parsed: Record<string, string> = {};
  for (const [key, entry] of Object.entries(value)) {
    if (typeof entry !== "string") {
      throw new Error(`event.data.${key} must be a string`);
    }
    parsed[key] = entry;
  }
  return parsed;
}

function parseEventType(value: unknown): SessionStreamEventType {
  if (typeof value !== "string" || !SESSION_STREAM_EVENT_TYPE_SET.has(value)) {
    throw new Error("unknown event_type");
  }
  return value as SessionStreamEventType;
}

export function parseSessionStreamEvent(raw: string): SessionStreamEvent {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error("invalid JSON");
  }
  if (!isJsonRecord(parsed)) {
    throw new Error("event payload must be an object");
  }

  if (parsed.type !== WS_EVENT_MESSAGE_TYPE) {
    throw new Error("unknown message type");
  }
  if (typeof parsed.session_id !== "string" || parsed.session_id.length === 0) {
    throw new Error("missing session_id");
  }

  const event_type = parseEventType(parsed.event_type);
  const data = parseStringRecord(parsed.data);
  return {
    type: WS_EVENT_MESSAGE_TYPE,
    event_type,
    session_id: parsed.session_id,
    data,
  } as SessionStreamEvent;
}
