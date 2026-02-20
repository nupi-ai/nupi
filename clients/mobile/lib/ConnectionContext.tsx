import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import { AppState, type AppStateStatus } from "react-native";

import { ConnectError, Code } from "@connectrpc/connect";

import {
  createNupiClientFromConfig,
  type NupiClient,
} from "./connect";
import { mapConnectionError } from "./errorMessages";
import {
  clearAll,
  getConnectionConfig,
  getToken,
  saveConnectionConfig,
  saveToken,
  type StoredConnectionConfig,
} from "./storage";
import { useNetworkStatus } from "./useNetworkStatus";

type ConnectionStatus = "disconnected" | "connecting" | "connected";

const MAX_RECONNECT_ATTEMPTS = 3;

interface ConnectionContextValue {
  status: ConnectionStatus;
  client: NupiClient | null;
  error: string | null;
  errorCanRetry: boolean;
  hostInfo: string | null;
  reconnecting: boolean;
  reconnectAttempts: number;
  connect: (config: StoredConnectionConfig, token: string) => Promise<void>;
  disconnect: () => Promise<void>;
  retryConnection: () => void;
}

const ConnectionContext = createContext<ConnectionContextValue>({
  status: "disconnected",
  client: null,
  error: null,
  errorCanRetry: true,
  hostInfo: null,
  reconnecting: false,
  reconnectAttempts: 0,
  connect: async () => {},
  disconnect: async () => {},
  retryConnection: () => {},
});

export function useConnection() {
  return useContext(ConnectionContext);
}

export function ConnectionProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<ConnectionStatus>("disconnected");
  const [client, setClient] = useState<NupiClient | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [errorCanRetry, setErrorCanRetry] = useState(true);
  const [hostInfo, setHostInfo] = useState<string | null>(null);
  const [reconnecting, setReconnecting] = useState(false);
  const [reconnectAttempts, setReconnectAttempts] = useState(0);
  const mountedRef = useRef(true);
  const manualConnectRef = useRef(false);
  const manualDisconnectRef = useRef(false);
  const isReconnectingRef = useRef(false);
  const reconnectAttemptsRef = useRef(0);
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const statusRef = useRef<ConnectionStatus>("disconnected");
  const { isConnected: networkConnected } = useNetworkStatus();
  const prevNetworkConnectedRef = useRef<boolean | null>(null);

  // Keep statusRef in sync with state for use in event handlers
  const updateStatus = useCallback((next: ConnectionStatus) => {
    statusRef.current = next;
    setStatus(next);
  }, []);

  const attemptReconnect = useCallback(async () => {
    if (isReconnectingRef.current || manualDisconnectRef.current) return;
    if (statusRef.current === "connected") return; // Already connected
    isReconnectingRef.current = true;
    if (mountedRef.current) {
      setReconnecting(true);
    }

    const token = await getToken();
    const config = await getConnectionConfig();

    if (!token || !config) {
      isReconnectingRef.current = false;
      if (mountedRef.current) {
        setReconnecting(false);
        setReconnectAttempts(0);
      }
      return;
    }

    try {
      const nupiClient = createNupiClientFromConfig(config);
      await nupiClient.daemon.status({});

      if (mountedRef.current && !manualDisconnectRef.current) {
        setClient(nupiClient);
        setHostInfo(`${config.host}:${config.port}`);
        updateStatus("connected");
        setError(null);
        setErrorCanRetry(true);
        reconnectAttemptsRef.current = 0;
        setReconnectAttempts(0);
        setReconnecting(false);
      }
      isReconnectingRef.current = false;
    } catch (err) {
      isReconnectingRef.current = false;

      if (!mountedRef.current || manualDisconnectRef.current) {
        if (mountedRef.current) setReconnecting(false);
        return;
      }

      // Auth errors — clear credentials, no retry (re-pair needed)
      if (
        err instanceof ConnectError &&
        (err.code === Code.Unauthenticated || err.code === Code.PermissionDenied)
      ) {
        await clearAll();
        updateStatus("disconnected");
        setError(mapConnectionError(err).message);
        setErrorCanRetry(false);
        reconnectAttemptsRef.current = 0;
        setReconnectAttempts(0);
        setReconnecting(false);
        return;
      }

      if (reconnectAttemptsRef.current < MAX_RECONNECT_ATTEMPTS) {
        // Increment retry counter and schedule next attempt with exponential backoff: 1s, 2s, 4s
        reconnectAttemptsRef.current += 1;
        if (mountedRef.current) {
          setReconnectAttempts(reconnectAttemptsRef.current);
        }
        const delay = 1000 * Math.pow(2, reconnectAttemptsRef.current - 1);
        reconnectTimerRef.current = setTimeout(() => {
          reconnectTimerRef.current = null;
          isReconnectingRef.current = false;
          attemptReconnect();
        }, delay);
      } else {
        // Max attempts reached
        updateStatus("disconnected");
        setError(`Cannot reach nupid at ${config.host}:${config.port}`);
        setErrorCanRetry(true);
        reconnectAttemptsRef.current = 0;
        setReconnectAttempts(0);
        setReconnecting(false);
      }
    }
  }, [updateStatus]);

  const retryConnection = useCallback(() => {
    // Clear previous state and start fresh reconnect cycle
    reconnectAttemptsRef.current = 0;
    if (reconnectTimerRef.current) {
      clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
    isReconnectingRef.current = false;
    if (mountedRef.current) {
      setReconnectAttempts(0);
      setError(null);
    }
    attemptReconnect();
  }, [attemptReconnect]);

  const connectWithConfig = useCallback(
    async (config: StoredConnectionConfig, token: string) => {
      manualConnectRef.current = true;
      manualDisconnectRef.current = false;
      updateStatus("connecting");
      setError(null);
      setErrorCanRetry(true);

      try {
        // Store credentials first so auth interceptor can read the token
        await saveToken(token);
        await saveConnectionConfig(config);

        const nupiClient = createNupiClientFromConfig(config);

        // Verify connection — auth interceptor reads token from SecureStore
        await nupiClient.daemon.status({});

        if (mountedRef.current) {
          setClient(nupiClient);
          setHostInfo(`${config.host}:${config.port}`);
          updateStatus("connected");
        }
      } catch (err) {
        // Verification failed — keep stored credentials so auto-reconnect
        // can retry (pairing codes are one-time use, token is still valid)
        if (mountedRef.current) {
          const mapped = mapConnectionError(err);
          updateStatus("disconnected");
          setError(mapped.message);
          setErrorCanRetry(mapped.canRetry);
        }
        throw err;
      } finally {
        manualConnectRef.current = false;
      }
    },
    [updateStatus]
  );

  const disconnect = useCallback(async () => {
    manualDisconnectRef.current = true;
    // Cancel any pending reconnect
    if (reconnectTimerRef.current) {
      clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
    isReconnectingRef.current = false;
    reconnectAttemptsRef.current = 0;

    if (mountedRef.current) {
      setClient(null);
      setHostInfo(null);
      updateStatus("disconnected");
      setError(null);
      setErrorCanRetry(true);
      setReconnecting(false);
      setReconnectAttempts(0);
    }
    await clearAll();
  }, [updateStatus]);

  // Auto-reconnect on mount using stored credentials
  useEffect(() => {
    mountedRef.current = true;

    (async () => {
      const token = await getToken();
      const config = await getConnectionConfig();

      if (!token || !config) return;

      if (mountedRef.current) updateStatus("connecting");

      try {
        const nupiClient = createNupiClientFromConfig(config);
        await nupiClient.daemon.status({});

        if (mountedRef.current && !manualConnectRef.current) {
          setClient(nupiClient);
          setHostInfo(`${config.host}:${config.port}`);
          updateStatus("connected");
        }
      } catch (err) {
        if (mountedRef.current && !manualConnectRef.current) {
          if (
            err instanceof ConnectError &&
            (err.code === Code.Unauthenticated || err.code === Code.PermissionDenied)
          ) {
            await clearAll();
          }
          const mapped = mapConnectionError(err);
          updateStatus("disconnected");
          setError(mapped.message);
          setErrorCanRetry(mapped.canRetry);
        }
      }
    })();

    return () => {
      mountedRef.current = false;
    };
  }, [updateStatus]);

  // AppState listener: reconnect when app comes to foreground
  useEffect(() => {
    const subscription = AppState.addEventListener(
      "change",
      (nextState: AppStateStatus) => {
        if (
          nextState === "active" &&
          statusRef.current === "disconnected" &&
          !manualDisconnectRef.current &&
          !isReconnectingRef.current &&
          prevNetworkConnectedRef.current === true
        ) {
          // Cancel any pending backoff timer and reset counters before starting new cycle
          if (reconnectTimerRef.current) {
            clearTimeout(reconnectTimerRef.current);
            reconnectTimerRef.current = null;
          }
          reconnectAttemptsRef.current = 0;
          setReconnectAttempts(0);
          // Only reconnect if currently disconnected with stored credentials
          (async () => {
            const token = await getToken();
            const config = await getConnectionConfig();
            // Re-check status after async reads — may have connected in the meantime
            const currentStatus: ConnectionStatus = statusRef.current;
            if (token && config && currentStatus === "disconnected" && !isReconnectingRef.current && !manualDisconnectRef.current) {
              attemptReconnect();
            }
          })();
        }
      }
    );

    return () => {
      subscription.remove();
    };
  }, [attemptReconnect]);

  // Network state listener: reconnect when network becomes available
  useEffect(() => {
    const prev = prevNetworkConnectedRef.current;
    prevNetworkConnectedRef.current = networkConnected;

    // Network lost while connected — mark disconnected and wait for recovery
    if (
      prev === true &&
      networkConnected === false &&
      statusRef.current === "connected" &&
      !manualDisconnectRef.current
    ) {
      updateStatus("disconnected");
      setReconnecting(true);
      setError(null);
      setErrorCanRetry(true);
      // Don't attempt reconnect — network is down, it would fail.
      // The false→true handler below will trigger reconnect when network returns.
    }

    // Trigger reconnect on false → true transition
    if (
      prev === false &&
      networkConnected === true &&
      statusRef.current !== "connected" &&
      !manualDisconnectRef.current &&
      !isReconnectingRef.current
    ) {
      // Cancel any pending backoff timer from a previous cycle
      if (reconnectTimerRef.current) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
      reconnectAttemptsRef.current = 0;
      isReconnectingRef.current = false;
      setReconnectAttempts(0);
      attemptReconnect();
    }
  }, [networkConnected, attemptReconnect, updateStatus]);

  // Cleanup timers on unmount
  useEffect(() => {
    return () => {
      if (reconnectTimerRef.current) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
    };
  }, []);

  const value = React.useMemo(
    () => ({
      status,
      client,
      error,
      errorCanRetry,
      hostInfo,
      reconnecting,
      reconnectAttempts,
      connect: connectWithConfig,
      disconnect,
      retryConnection,
    }),
    [status, client, error, errorCanRetry, hostInfo, reconnecting, reconnectAttempts, connectWithConfig, disconnect, retryConnection]
  );

  return (
    <ConnectionContext.Provider value={value}>
      {children}
    </ConnectionContext.Provider>
  );
}
