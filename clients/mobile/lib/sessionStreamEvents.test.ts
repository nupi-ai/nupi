import { describe, expect, it } from "vitest";

import { parseSessionStreamEvent } from "./sessionStreamEvents";

describe("parseSessionStreamEvent", () => {
  it("parses stopped event", () => {
    const event = parseSessionStreamEvent(
      JSON.stringify({
        type: "event",
        event_type: "stopped",
        session_id: "s1",
        data: { exit_code: "0", reason: "process_exit" },
      })
    );

    expect(event.event_type).toBe("stopped");
    expect(event.session_id).toBe("s1");
    expect(event.data?.exit_code).toBe("0");
  });

  it("parses resize_instruction event", () => {
    const event = parseSessionStreamEvent(
      JSON.stringify({
        type: "event",
        event_type: "resize_instruction",
        session_id: "s2",
        data: { cols: "120", rows: "40" },
      })
    );

    expect(event.event_type).toBe("resize_instruction");
    expect(event.data?.cols).toBe("120");
    expect(event.data?.rows).toBe("40");
  });

  it("rejects unknown event_type", () => {
    expect(() =>
      parseSessionStreamEvent(
        JSON.stringify({
          type: "event",
          event_type: "unknown",
          session_id: "s3",
        })
      )
    ).toThrow("unknown event_type");
  });

  it("rejects invalid data shape", () => {
    expect(() =>
      parseSessionStreamEvent(
        JSON.stringify({
          type: "event",
          event_type: "created",
          session_id: "s4",
          data: { exit_code: 0 },
        })
      )
    ).toThrow("must be a string");
  });
});
