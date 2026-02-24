import { ConnectError } from "@connectrpc/connect";
import { connectCodeToErrorCode, ErrorCode } from "@nupi/shared/errors";

export type ErrorAction = "retry" | "re-pair" | "go-back" | "none";

export interface MappedError {
  message: string;
  action: ErrorAction;
  canRetry: boolean;
}

export type ErrorLike = string | MappedError;

export function toMappedError(error: ErrorLike): MappedError {
  if (typeof error === "string") {
    return {
      message: error,
      action: "none",
      canRetry: false,
    };
  }
  return error;
}

export function resolveMappedErrorAction(mapped: MappedError): ErrorAction {
  if (mapped.action === "retry" && !mapped.canRetry) return "none";
  return mapped.action;
}

/**
 * Maps connection errors (ConnectError, generic Error, unknown) to
 * user-friendly messages with a suggested recovery action.
 *
 * Consolidates duplicated error-mapping logic from:
 *  - ConnectionContext.tsx (inline ConnectError check)
 *  - sessions.tsx (mapSessionsError)
 *  - pairing.ts (mapPairingError)
 */
export function mapConnectionError(err: unknown): MappedError {
  if (err instanceof ConnectError) {
    switch (connectCodeToErrorCode(err.code)) {
      case ErrorCode.Unauthenticated:
        return {
          message: "Session expired \u2014 please re-pair.",
          action: "re-pair",
          canRetry: false,
        };
      case ErrorCode.PermissionDenied:
        return {
          message: "Permission denied \u2014 please re-pair.",
          action: "re-pair",
          canRetry: false,
        };
      case ErrorCode.Unavailable:
        return {
          message: "Cannot reach nupid \u2014 check your network.",
          action: "retry",
          canRetry: true,
        };
      case ErrorCode.NotFound:
        return {
          message: "Session not found \u2014 it may have ended.",
          action: "go-back",
          canRetry: false,
        };
      case ErrorCode.FailedPrecondition:
        return {
          message: "Pairing code expired. Generate a new one.",
          action: "re-pair",
          canRetry: false,
        };
      case ErrorCode.DeadlineExceeded:
        return {
          message: "Request timed out \u2014 please try again.",
          action: "retry",
          canRetry: true,
        };
      default:
        return {
          message: "Something went wrong \u2014 please try again.",
          action: "retry",
          canRetry: true,
        };
    }
  }
  if (err instanceof Error) {
    return {
      message: "Connection error \u2014 please try again.",
      action: "retry",
      canRetry: true,
    };
  }
  return {
    message: "An unexpected error occurred.",
    action: "retry",
    canRetry: true,
  };
}
