/** Audio format descriptor sent by the daemon via audio_meta text frames. */
export interface AudioFormat {
  encoding: string;
  sample_rate: number;
  channels: number;
  bit_depth: number;
}

/** Capability info for a single audio stream (from daemon capabilities_response). */
export interface AudioCapInfo {
  stream_id: string;
  format?: AudioFormat;
  ready: boolean;
}

/** Parsed control messages from daemon text frames. */
export type AudioControlMessage =
  | { type: "audio_meta"; format: AudioFormat; sequence: number; first: boolean; last: boolean; duration_ms: number }
  | { type: "capabilities_response"; capture: AudioCapInfo[]; playback: AudioCapInfo[] }
  | { type: "close_stream" }
  | { type: "unknown"; raw: string };

const DEFAULT_FORMAT: AudioFormat = { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 };

/** Validate and parse an AudioFormat object, falling back to defaults for invalid fields. */
function parseAudioFormat(raw: unknown): AudioFormat {
  if (typeof raw !== "object" || raw === null) return DEFAULT_FORMAT;
  const f = raw as Record<string, unknown>;
  return {
    encoding: typeof f.encoding === "string" ? f.encoding : DEFAULT_FORMAT.encoding,
    sample_rate: typeof f.sample_rate === "number" && f.sample_rate > 0 ? f.sample_rate : DEFAULT_FORMAT.sample_rate,
    channels: typeof f.channels === "number" && f.channels > 0 ? f.channels : DEFAULT_FORMAT.channels,
    bit_depth: typeof f.bit_depth === "number" && f.bit_depth > 0 ? f.bit_depth : DEFAULT_FORMAT.bit_depth,
  };
}

/** Validate and parse an AudioCapInfo object, returning null for invalid entries. */
function parseAudioCapInfo(raw: unknown): AudioCapInfo | null {
  if (typeof raw !== "object" || raw === null) return null;
  const obj = raw as Record<string, unknown>;
  if (typeof obj.stream_id !== "string") return null;
  return {
    stream_id: obj.stream_id,
    format: obj.format ? parseAudioFormat(obj.format) : undefined,
    ready: typeof obj.ready === "boolean" ? obj.ready : false,
  };
}

/**
 * Parse a text frame from the audio WebSocket into a typed control message.
 */
export function parseAudioMessage(data: string): AudioControlMessage {
  let msg: unknown;
  try {
    msg = JSON.parse(data);
  } catch {
    return { type: "unknown", raw: data };
  }
  if (typeof msg !== "object" || msg === null || typeof (msg as Record<string, unknown>).type !== "string") {
    return { type: "unknown", raw: data };
  }
  const obj = msg as Record<string, unknown>;
  switch (obj.type) {
    case "audio_meta":
      return {
        type: "audio_meta",
        format: parseAudioFormat(obj.format),
        sequence: typeof obj.sequence === "number" ? obj.sequence : 0,
        first: typeof obj.first === "boolean" ? obj.first : false,
        last: typeof obj.last === "boolean" ? obj.last : false,
        duration_ms: typeof obj.duration_ms === "number" ? obj.duration_ms : 0,
      };
    case "capabilities_response":
      return {
        type: "capabilities_response",
        capture: Array.isArray(obj.capture)
          ? obj.capture.map(parseAudioCapInfo).filter((x): x is AudioCapInfo => x !== null)
          : [],
        playback: Array.isArray(obj.playback)
          ? obj.playback.map(parseAudioCapInfo).filter((x): x is AudioCapInfo => x !== null)
          : [],
      };
    case "close_stream":
      return { type: "close_stream" };
    default:
      return { type: "unknown", raw: data };
  }
}

/**
 * Convert a PCM s16le ArrayBuffer to Float32Array for Web Audio API.
 */
export function pcmS16leToFloat32(pcmBuffer: ArrayBuffer): Float32Array {
  const int16 = new Int16Array(pcmBuffer);
  const float32 = new Float32Array(int16.length);
  for (let i = 0; i < int16.length; i++) {
    float32[i] = int16[i]! / 32768.0;
  }
  return float32;
}
