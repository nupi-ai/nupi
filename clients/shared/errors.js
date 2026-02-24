export const ErrorCode = {
  Unauthenticated: "UNAUTHENTICATED",
  PermissionDenied: "PERMISSION_DENIED",
  Unavailable: "UNAVAILABLE",
  NotFound: "NOT_FOUND",
  FailedPrecondition: "FAILED_PRECONDITION",
  DeadlineExceeded: "DEADLINE_EXCEEDED",
};

const CONNECT_CODE_TO_ERROR_CODE = {
  4: ErrorCode.DeadlineExceeded,
  5: ErrorCode.NotFound,
  7: ErrorCode.PermissionDenied,
  9: ErrorCode.FailedPrecondition,
  14: ErrorCode.Unavailable,
  16: ErrorCode.Unauthenticated,
};

export function connectCodeToErrorCode(code) {
  return CONNECT_CODE_TO_ERROR_CODE[code] ?? null;
}

export function isAuthErrorCode(code) {
  return code === ErrorCode.Unauthenticated || code === ErrorCode.PermissionDenied;
}
