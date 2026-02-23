import { ConnectError, Code } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import {
  mapConnectionError,
  resolveMappedErrorAction,
  toMappedError,
  type MappedError,
} from "./errorMessages";

describe("errorMessages helpers", () => {
  it("maps plain string to non-actionable mapped error", () => {
    expect(toMappedError("custom error")).toEqual({
      message: "custom error",
      action: "none",
      canRetry: false,
    });
  });

  it("keeps mapped error unchanged", () => {
    const mapped: MappedError = {
      message: "already mapped",
      action: "retry",
      canRetry: true,
    };

    expect(toMappedError(mapped)).toBe(mapped);
  });

  it("normalizes retry action when retry is not allowed", () => {
    expect(
      resolveMappedErrorAction({
        message: "cannot retry",
        action: "retry",
        canRetry: false,
      })
    ).toBe("none");
  });

  it("maps connect unauthenticated error to re-pair action", () => {
    const mapped = mapConnectionError(new ConnectError("unauth", Code.Unauthenticated));

    expect(mapped.action).toBe("re-pair");
    expect(mapped.canRetry).toBe(false);
    expect(mapped.message).toContain("re-pair");
  });
});
