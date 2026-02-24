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

/**
 * Parse a text frame from the audio WebSocket into a typed control message.
 */
export function parseAudioMessage(data: string): AudioControlMessage {
  const msg = JSON.parse(data);
  if (typeof msg !== "object" || msg === null || typeof msg.type !== "string") {
    return { type: "unknown", raw: data };
  }
  switch (msg.type) {
    case "audio_meta":
      return {
        type: "audio_meta",
        format: msg.format ?? { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 },
        sequence: msg.sequence ?? 0,
        first: msg.first ?? false,
        last: msg.last ?? false,
        duration_ms: msg.duration_ms ?? 0,
      };
    case "capabilities_response":
      return {
        type: "capabilities_response",
        capture: Array.isArray(msg.capture) ? msg.capture : [],
        playback: Array.isArray(msg.playback) ? msg.playback : [],
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
