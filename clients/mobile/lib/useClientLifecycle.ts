import React, { useCallback, useEffect } from "react";

import { createNupiClientFromConfig } from "./connect";
import {
  isAuthError,
  readStoredCredentials,
  type ConnectionRuntimeRefs,
  type ConnectionStateActions,
} from "./connectionUtils";
import { mapConnectionError } from "./errorMessages";
import { unregisterPushTokenWithToken, cancelInflightRegistration } from "./notifications";
import { raceTimeout } from "./raceTimeout";
import {
  clearAll,
  getToken,
  saveConnectionConfig,
  saveToken,
  type StoredConnectionConfig,
} from "./storage";

const STATUS_TIMEOUT_MS = 5000;

interface UseClientLifecycleParams {
  actions: ConnectionStateActions;
  refs: ConnectionRuntimeRefs;
  /** M5 fix (Review 13): ref to a callback invoked synchronously before
   * unregisterPushToken to cancel any pending registration retry in
   * NotificationContext. Set by NotificationProvider via ConnectionContext. */
  onBeforeDisconnectRef: React.MutableRefObject<(() => void) | null>;
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
  onBeforeDisconnectRef,
}: UseClientLifecycleParams): UseClientLifecycleResult {
  const {
    setClient,
    setError,
    setErrorAction,
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
    clientRef,
  } = refs;

  const connectWithConfig = useCallback(
    async (config: StoredConnectionConfig, token: string) => {
      manualConnectRef.current = true;
      manualDisconnectRef.current = false;
      updateStatus("connecting");
      setError(null);
      setErrorAction("none");
      setErrorCanRetry(true);

      try {
        // Store credentials first so auth interceptor can read the token.
        await saveToken(token);
        await saveConnectionConfig(config);

        const nupiClient = createNupiClientFromConfig(config);

        // Verify connection — auth interceptor reads token from SecureStore.
        await raceTimeout(nupiClient.daemon.status({}), STATUS_TIMEOUT_MS, "daemon status check timed out");

        setClient(nupiClient);
        setHostInfo(`${config.host}:${config.port}`);
        updateStatus("connected");
        setErrorAction("none");
      } catch (err) {
        // Keep stored credentials so reconnect can retry one-time pairing flow.
        const mapped = mapConnectionError(err);
        updateStatus("disconnected");
        setError(mapped.message);
        setErrorAction(mapped.action);
        setErrorCanRetry(mapped.canRetry);
        throw err;
      } finally {
        manualConnectRef.current = false;
      }
    },
    [
      manualConnectRef,
      manualDisconnectRef,
      setClient,
      setError,
      setErrorAction,
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

    // M5 fix (Review 13): cancel pending registration retry before unregister
    // to prevent a race where the retry re-registers after unregister fires.
    onBeforeDisconnectRef.current?.();
    // H3 fix (Review 14): abort any in-flight registerPushToken RPC. Without
    // this, a slow register could complete AFTER unregister, leaving the daemon
    // with a token the client thinks was unregistered.
    cancelInflightRegistration();

    // C1 fix (Review 13): Capture auth token synchronously BEFORE clearAll()
    // wipes it. Pass the captured token directly to the unregister RPC so
    // the fire-and-forget call doesn't race with credential clearing.
    const currentClient = clientRef.current;
    const capturedToken = await getToken();
    if (currentClient && capturedToken) {
      // Fire-and-forget: don't block disconnect on unregister RPC.
      // If daemon is unreachable (common disconnect reason), awaiting would
      // delay the UI by the full raceTimeout (5s). Daemon cleans up stale
      // tokens via DeviceNotRegistered on next push attempt anyway.
      unregisterPushTokenWithToken(currentClient, capturedToken).catch((err) => {
        console.warn("[Notifications] unregisterPushToken on disconnect:", err);
      });
    }

    setClient(null);
    setHostInfo(null);
    updateStatus("disconnected");
    setError(null);
    setErrorAction("none");
    setErrorCanRetry(true);
    setReconnecting(false);
    setReconnectAttempts(0);

    await clearAll();
  }, [
    clientRef,
    isReconnectingRef,
    manualDisconnectRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
    setClient,
    setError,
    setErrorAction,
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

      updateStatus("connecting");

      try {
        const nupiClient = createNupiClientFromConfig(config);
        await raceTimeout(nupiClient.daemon.status({}), STATUS_TIMEOUT_MS, "daemon status check timed out");

        if (mountedRef.current && !manualConnectRef.current) {
          setClient(nupiClient);
          setHostInfo(`${config.host}:${config.port}`);
          updateStatus("connected");
          setErrorAction("none");
        }
      } catch (err) {
        if (mountedRef.current && !manualConnectRef.current) {
          if (isAuthError(err)) {
            await clearAll();
          }
          const mapped = mapConnectionError(err);
          updateStatus("disconnected");
          setError(mapped.message);
          setErrorAction(mapped.action);
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
    setErrorAction,
    setErrorCanRetry,
    setHostInfo,
    updateStatus,
  ]);

  return { connectWithConfig, disconnect };
}
