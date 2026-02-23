import { isJsonRecord, parseStringRecordStrict } from "@nupi/shared/json";
import { createEventParser, parseSessionId } from "@nupi/shared/events";

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

const parseSessionStreamEventType = createEventParser(SESSION_STREAM_EVENT_TYPES);

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

function parseStringRecord(value: unknown): Record<string, string> | undefined {
  if (value === undefined) {
    return undefined;
  }
  return parseStringRecordStrict(value, "event.data");
}

function parseEventType(value: unknown): SessionStreamEventType {
  const parsed = parseSessionStreamEventType(value);
  if (parsed === null) {
    throw new Error("unknown event_type");
  }
  return parsed;
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
  const session_id = parseSessionId(parsed.session_id);
  if (session_id === null) {
    throw new Error("missing session_id");
  }

  const event_type = parseEventType(parsed.event_type);
  const data = parseStringRecord(parsed.data);
  return {
    type: WS_EVENT_MESSAGE_TYPE,
    event_type,
    session_id,
    data,
  } as SessionStreamEvent;
}
