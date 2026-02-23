import type { Dispatch, MutableRefObject, SetStateAction } from "react";

import { Code, ConnectError } from "@connectrpc/connect";

import type { NupiClient } from "./connect";
import type { ErrorAction } from "./errorMessages";
import { getConnectionConfig, getToken, type StoredConnectionConfig } from "./storage";

export type ConnectionStatus = "disconnected" | "connecting" | "connected";

export const MAX_RECONNECT_ATTEMPTS = 3;

export interface ConnectionStateActions {
  setClient: Dispatch<SetStateAction<NupiClient | null>>;
  setError: Dispatch<SetStateAction<string | null>>;
  setErrorAction: Dispatch<SetStateAction<ErrorAction>>;
  setErrorCanRetry: Dispatch<SetStateAction<boolean>>;
  setHostInfo: Dispatch<SetStateAction<string | null>>;
  setReconnecting: Dispatch<SetStateAction<boolean>>;
  setReconnectAttempts: Dispatch<SetStateAction<number>>;
  updateStatus: (next: ConnectionStatus) => void;
}

export interface ConnectionRuntimeRefs {
  mountedRef: MutableRefObject<boolean>;
  manualConnectRef: MutableRefObject<boolean>;
  manualDisconnectRef: MutableRefObject<boolean>;
  isReconnectingRef: MutableRefObject<boolean>;
  reconnectAttemptsRef: MutableRefObject<number>;
  reconnectTimerRef: MutableRefObject<ReturnType<typeof setTimeout> | null>;
  statusRef: MutableRefObject<ConnectionStatus>;
  prevNetworkConnectedRef: MutableRefObject<boolean | null>;
}

export interface StoredCredentials {
  token: string | null;
  config: StoredConnectionConfig | null;
}

export async function readStoredCredentials(): Promise<StoredCredentials> {
  const [token, config] = await Promise.all([getToken(), getConnectionConfig()]);
  return { token, config };
}

export function isAuthError(err: unknown): err is ConnectError {
  return (
    err instanceof ConnectError &&
    (err.code === Code.Unauthenticated || err.code === Code.PermissionDenied)
  );
}
