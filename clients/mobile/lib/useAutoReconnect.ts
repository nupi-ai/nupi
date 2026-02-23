import { useCallback, useEffect } from "react";
import { AppState, type AppStateStatus } from "react-native";

import { createNupiClientFromConfig } from "./connect";
import {
  isAuthError,
  MAX_RECONNECT_ATTEMPTS,
  readStoredCredentials,
  type ConnectionRuntimeRefs,
  type ConnectionStateActions,
} from "./connectionUtils";
import { mapConnectionError } from "./errorMessages";
import { clearAll } from "./storage";

interface UseAutoReconnectParams {
  networkConnected: boolean | null;
  actions: ConnectionStateActions;
  refs: ConnectionRuntimeRefs;
}

interface UseAutoReconnectResult {
  retryConnection: () => void;
}

export function useAutoReconnect({
  networkConnected,
  actions,
  refs,
}: UseAutoReconnectParams): UseAutoReconnectResult {
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
    manualDisconnectRef,
    isReconnectingRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
    statusRef,
    prevNetworkConnectedRef,
  } = refs;

  const attemptReconnect = useCallback(async () => {
    if (isReconnectingRef.current || manualDisconnectRef.current) return;
    if (statusRef.current === "connected") return;

    isReconnectingRef.current = true;
    setReconnecting(true);

    const { token, config } = await readStoredCredentials();
    if (!token || !config) {
      isReconnectingRef.current = false;
      setReconnecting(false);
      setReconnectAttempts(0);
      return;
    }

    try {
      const nupiClient = createNupiClientFromConfig(config);
      await nupiClient.daemon.status({});

      if (!manualDisconnectRef.current) {
        setClient(nupiClient);
        setHostInfo(`${config.host}:${config.port}`);
        updateStatus("connected");
        setError(null);
        setErrorAction("none");
        setErrorCanRetry(true);
        reconnectAttemptsRef.current = 0;
        setReconnectAttempts(0);
        setReconnecting(false);
      }
      isReconnectingRef.current = false;
    } catch (err) {
      isReconnectingRef.current = false;

      if (!mountedRef.current || manualDisconnectRef.current) {
        setReconnecting(false);
        return;
      }

      if (isAuthError(err)) {
        const mapped = mapConnectionError(err);
        await clearAll();
        updateStatus("disconnected");
        setError(mapped.message);
        setErrorAction(mapped.action);
        setErrorCanRetry(false);
        reconnectAttemptsRef.current = 0;
        setReconnectAttempts(0);
        setReconnecting(false);
        return;
      }

      if (reconnectAttemptsRef.current < MAX_RECONNECT_ATTEMPTS) {
        reconnectAttemptsRef.current += 1;
        setReconnectAttempts(reconnectAttemptsRef.current);
        const delay = 1000 * Math.pow(2, reconnectAttemptsRef.current - 1);
        reconnectTimerRef.current = setTimeout(() => {
          reconnectTimerRef.current = null;
          isReconnectingRef.current = false;
          attemptReconnect();
        }, delay);
      } else {
        updateStatus("disconnected");
        setError(`Cannot reach nupid at ${config.host}:${config.port}`);
        setErrorAction("retry");
        setErrorCanRetry(true);
        reconnectAttemptsRef.current = 0;
        setReconnectAttempts(0);
        setReconnecting(false);
      }
    }
  }, [
    isReconnectingRef,
    manualDisconnectRef,
    mountedRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
    setClient,
    setError,
    setErrorAction,
    setErrorCanRetry,
    setHostInfo,
    setReconnectAttempts,
    setReconnecting,
    statusRef,
    updateStatus,
  ]);

  const retryConnection = useCallback(() => {
    reconnectAttemptsRef.current = 0;
    if (reconnectTimerRef.current) {
      clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
    isReconnectingRef.current = false;
    setReconnectAttempts(0);
    setError(null);
    setErrorAction("none");
    attemptReconnect();
  }, [
    attemptReconnect,
    isReconnectingRef,
    mountedRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
    setError,
    setErrorAction,
    setReconnectAttempts,
  ]);

  // Reconnect when app returns to foreground.
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
          if (reconnectTimerRef.current) {
            clearTimeout(reconnectTimerRef.current);
            reconnectTimerRef.current = null;
          }
          reconnectAttemptsRef.current = 0;
          setReconnectAttempts(0);

          (async () => {
            const { token, config } = await readStoredCredentials();
            const currentStatus = statusRef.current;
            if (
              token &&
              config &&
              currentStatus === "disconnected" &&
              !isReconnectingRef.current &&
              !manualDisconnectRef.current
            ) {
              attemptReconnect();
            }
          })();
        }
      }
    );

    return () => {
      subscription.remove();
    };
  }, [
    attemptReconnect,
    isReconnectingRef,
    manualDisconnectRef,
    prevNetworkConnectedRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
    setReconnectAttempts,
    statusRef,
  ]);

  // Reconnect when network returns.
  useEffect(() => {
    const prev = prevNetworkConnectedRef.current;
    prevNetworkConnectedRef.current = networkConnected;

    if (
      prev === true &&
      networkConnected === false &&
      statusRef.current === "connected" &&
      !manualDisconnectRef.current
    ) {
      updateStatus("disconnected");
      setReconnecting(true);
      setError(null);
      setErrorAction("none");
      setErrorCanRetry(true);
    }

    if (
      prev === false &&
      networkConnected === true &&
      statusRef.current !== "connected" &&
      !manualDisconnectRef.current &&
      !isReconnectingRef.current
    ) {
      if (reconnectTimerRef.current) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
      reconnectAttemptsRef.current = 0;
      isReconnectingRef.current = false;
      setReconnectAttempts(0);
      attemptReconnect();
    }
  }, [
    attemptReconnect,
    isReconnectingRef,
    manualDisconnectRef,
    networkConnected,
    prevNetworkConnectedRef,
    reconnectAttemptsRef,
    reconnectTimerRef,
    setError,
    setErrorAction,
    setErrorCanRetry,
    setReconnectAttempts,
    setReconnecting,
    statusRef,
    updateStatus,
  ]);

  // Clear pending timer on unmount.
  useEffect(() => {
    return () => {
      if (reconnectTimerRef.current) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
    };
  }, [reconnectTimerRef]);

  return { retryConnection };
}
