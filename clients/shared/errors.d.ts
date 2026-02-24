export declare const ErrorCode: {
  readonly Unauthenticated: "UNAUTHENTICATED";
  readonly PermissionDenied: "PERMISSION_DENIED";
  readonly Unavailable: "UNAVAILABLE";
  readonly NotFound: "NOT_FOUND";
  readonly FailedPrecondition: "FAILED_PRECONDITION";
  readonly DeadlineExceeded: "DEADLINE_EXCEEDED";
};

export type ErrorCodeValue = (typeof ErrorCode)[keyof typeof ErrorCode];

export declare function connectCodeToErrorCode(code: number): ErrorCodeValue | null;
export declare function isAuthErrorCode(code: ErrorCodeValue | null): boolean;
