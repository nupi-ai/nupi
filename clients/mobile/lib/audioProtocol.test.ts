import { describe, expect, it } from "vitest";

import { parseAudioMessage, pcmS16leToFloat32 } from "./audioProtocol";

describe("parseAudioMessage", () => {
  it("parses audio_meta with full format", () => {
    const msg = parseAudioMessage(
      JSON.stringify({
        type: "audio_meta",
        sequence: 1,
        first: true,
        last: false,
        duration_ms: 20,
        format: {
          encoding: "pcm_s16le",
          sample_rate: 24000,
          channels: 1,
          bit_depth: 16,
        },
      })
    );
    expect(msg).toEqual({
      type: "audio_meta",
      sequence: 1,
      first: true,
      last: false,
      duration_ms: 20,
      format: {
        encoding: "pcm_s16le",
        sample_rate: 24000,
        channels: 1,
        bit_depth: 16,
      },
    });
  });

  it("provides defaults for missing audio_meta fields", () => {
    const msg = parseAudioMessage(JSON.stringify({ type: "audio_meta" }));
    expect(msg).toEqual({
      type: "audio_meta",
      sequence: 0,
      first: false,
      last: false,
      duration_ms: 0,
      format: {
        encoding: "pcm_s16le",
        sample_rate: 16000,
        channels: 1,
        bit_depth: 16,
      },
    });
  });

  it("parses capabilities_response with playback streams", () => {
    const msg = parseAudioMessage(
      JSON.stringify({
        type: "capabilities_response",
        capture: [{ stream_id: "mic", format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 }, ready: true }],
        playback: [{ stream_id: "tts", format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 }, ready: true }],
      })
    );
    expect(msg).toEqual({
      type: "capabilities_response",
      capture: [{ stream_id: "mic", format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 }, ready: true }],
      playback: [{ stream_id: "tts", format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 }, ready: true }],
    });
  });

  it("parses capabilities_response with empty arrays", () => {
    const msg = parseAudioMessage(JSON.stringify({ type: "capabilities_response" }));
    expect(msg).toEqual({
      type: "capabilities_response",
      capture: [],
      playback: [],
    });
  });

  it("parses close_stream", () => {
    const msg = parseAudioMessage(JSON.stringify({ type: "close_stream" }));
    expect(msg).toEqual({ type: "close_stream" });
  });

  it("returns unknown for unrecognized type", () => {
    const raw = JSON.stringify({ type: "some_other_thing", data: 42 });
    const msg = parseAudioMessage(raw);
    expect(msg).toEqual({ type: "unknown", raw });
  });

  it("returns unknown for missing type field", () => {
    const raw = JSON.stringify({ foo: "bar" });
    const msg = parseAudioMessage(raw);
    expect(msg).toEqual({ type: "unknown", raw });
  });

  it("returns unknown for invalid JSON", () => {
    const msg = parseAudioMessage("not json");
    expect(msg).toEqual({ type: "unknown", raw: "not json" });
  });

  it("falls back to defaults for wrong-type audio_meta top-level fields", () => {
    const msg = parseAudioMessage(
      JSON.stringify({
        type: "audio_meta",
        sequence: "not_a_number",
        first: 123,
        last: "yes",
        duration_ms: null,
        format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1, bit_depth: 16 },
      })
    );
    expect(msg).toEqual({
      type: "audio_meta",
      sequence: 0,
      first: false,
      last: false,
      duration_ms: 0,
      format: {
        encoding: "pcm_s16le",
        sample_rate: 16000,
        channels: 1,
        bit_depth: 16,
      },
    });
  });

  it("filters invalid entries from capabilities_response arrays", () => {
    const msg = parseAudioMessage(
      JSON.stringify({
        type: "capabilities_response",
        capture: [
          { stream_id: "mic", ready: true },
          "not_an_object",
          { no_stream_id: true },
          null,
        ],
        playback: [
          { stream_id: "tts", ready: false },
          42,
        ],
      })
    );
    expect(msg).toEqual({
      type: "capabilities_response",
      capture: [{ stream_id: "mic", format: undefined, ready: true }],
      playback: [{ stream_id: "tts", format: undefined, ready: false }],
    });
  });

  it("falls back to defaults for malformed audio_meta format fields", () => {
    const msg = parseAudioMessage(
      JSON.stringify({
        type: "audio_meta",
        sequence: 1,
        first: true,
        last: false,
        duration_ms: 20,
        format: { encoding: 123, sample_rate: "abc", channels: null, bit_depth: -1 },
      })
    );
    expect(msg).toEqual({
      type: "audio_meta",
      sequence: 1,
      first: true,
      last: false,
      duration_ms: 20,
      format: {
        encoding: "pcm_s16le",
        sample_rate: 16000,
        channels: 1,
        bit_depth: 16,
      },
    });
  });
});

describe("pcmS16leToFloat32", () => {
  it("converts silence (zeros) to zero floats", () => {
    const pcm = new Int16Array([0, 0, 0]).buffer;
    const result = pcmS16leToFloat32(pcm);
    expect(result.length).toBe(3);
    expect(result[0]).toBe(0);
    expect(result[1]).toBe(0);
    expect(result[2]).toBe(0);
  });

  it("converts max positive Int16 to ~1.0", () => {
    const pcm = new Int16Array([32767]).buffer;
    const result = pcmS16leToFloat32(pcm);
    expect(result[0]).toBeCloseTo(1.0, 4);
  });

  it("converts min negative Int16 to -1.0", () => {
    const pcm = new Int16Array([-32768]).buffer;
    const result = pcmS16leToFloat32(pcm);
    expect(result[0]).toBe(-1.0);
  });

  it("handles typical audio values", () => {
    const pcm = new Int16Array([16384, -16384]).buffer;
    const result = pcmS16leToFloat32(pcm);
    expect(result[0]).toBeCloseTo(0.5, 4);
    expect(result[1]).toBeCloseTo(-0.5, 4);
  });

  it("handles empty buffer", () => {
    const pcm = new Int16Array([]).buffer;
    const result = pcmS16leToFloat32(pcm);
    expect(result.length).toBe(0);
  });
});
