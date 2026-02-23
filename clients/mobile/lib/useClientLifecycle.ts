import { useCallback, useEffect } from "react";

import { createNupiClientFromConfig } from "./connect";
import {
  isAuthError,
  readStoredCredentials,
  type ConnectionRuntimeRefs,
  type ConnectionStateActions,
} from "./connectionUtils";
import { mapConnectionError } from "./errorMessages";
import {
  clearAll,
  saveConnectionConfig,
  saveToken,
  type StoredConnectionConfig,
} from "./storage";

interface UseClientLifecycleParams {
  actions: ConnectionStateActions;
  refs: ConnectionRuntimeRefs;
}

interface UseClientLifecycleResult {
  connectWithConfig: (
    config: StoredConnectionConfig,
    token: string
  ) => Promise<void>;
  disconnect: () => Promise<void>;
}

export function useClientLifecycle({
  actions,
  refs,
}: UseClientLifecycleParams): UseClientLifecycleResult {
  const {
    setClient,
    setError,
    setErrorCanRetry,
    setHostInfo,
    setReconnecting,
    setReconnectAttempts,
    updateStatus,
  } = actions;

  const {
    mountedRef,
    manualConnectRef,
    manualDisconnectRef,
    isReconnectingRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
  } = refs;

  const connectWithConfig = useCallback(
    async (config: StoredConnectionConfig, token: string) => {
      manualConnectRef.current = true;
      manualDisconnectRef.current = false;
      updateStatus("connecting");
      setError(null);
      setErrorCanRetry(true);

      try {
        // Store credentials first so auth interceptor can read the token.
        await saveToken(token);
        await saveConnectionConfig(config);

        const nupiClient = createNupiClientFromConfig(config);

        // Verify connection — auth interceptor reads token from SecureStore.
        await nupiClient.daemon.status({});

        if (mountedRef.current) {
          setClient(nupiClient);
          setHostInfo(`${config.host}:${config.port}`);
          updateStatus("connected");
        }
      } catch (err) {
        // Keep stored credentials so reconnect can retry one-time pairing flow.
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
    [
      manualConnectRef,
      manualDisconnectRef,
      mountedRef,
      setClient,
      setError,
      setErrorCanRetry,
      setHostInfo,
      updateStatus,
    ]
  );

  const disconnect = useCallback(async () => {
    manualDisconnectRef.current = true;

    // Cancel any pending reconnect.
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
  }, [
    isReconnectingRef,
    manualDisconnectRef,
    mountedRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
    setClient,
    setError,
    setErrorCanRetry,
    setHostInfo,
    setReconnectAttempts,
    setReconnecting,
    updateStatus,
  ]);

  // Auto-connect on mount using stored credentials.
  useEffect(() => {
    mountedRef.current = true;

    (async () => {
      const { token, config } = await readStoredCredentials();

      if (!token || !config) return;

      if (mountedRef.current) {
        updateStatus("connecting");
      }

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
          if (isAuthError(err)) {
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
  }, [
    manualConnectRef,
    mountedRef,
    setClient,
    setError,
    setErrorCanRetry,
    setHostInfo,
    updateStatus,
  ]);

  return { connectWithConfig, disconnect };
}
