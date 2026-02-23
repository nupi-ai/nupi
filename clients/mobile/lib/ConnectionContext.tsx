import React, {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";

import type { NupiClient } from "./connect";
import {
  type ConnectionRuntimeRefs,
  type ConnectionStateActions,
  type ConnectionStatus,
} from "./connectionUtils";
import type { StoredConnectionConfig } from "./storage";
import { useAutoReconnect } from "./useAutoReconnect";
import { useClientLifecycle } from "./useClientLifecycle";
import { useNetworkStatus } from "./useNetworkStatus";

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
  const prevNetworkConnectedRef = useRef<boolean | null>(null);

  const { isConnected: networkConnected } = useNetworkStatus();

  const updateStatus = useCallback((next: ConnectionStatus) => {
    statusRef.current = next;
    setStatus(next);
  }, []);

  const actions: ConnectionStateActions = useMemo(
    () => ({
      setClient,
      setError,
      setErrorCanRetry,
      setHostInfo,
      setReconnecting,
      setReconnectAttempts,
      updateStatus,
    }),
    [updateStatus]
  );

  const refs: ConnectionRuntimeRefs = useMemo(
    () => ({
      mountedRef,
      manualConnectRef,
      manualDisconnectRef,
      isReconnectingRef,
      reconnectAttemptsRef,
      reconnectTimerRef,
      statusRef,
      prevNetworkConnectedRef,
    }),
    []
  );

  const { connectWithConfig, disconnect } = useClientLifecycle({ actions, refs });
  const { retryConnection } = useAutoReconnect({
    networkConnected,
    actions,
    refs,
  });

  const value = useMemo(
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
    [
      status,
      client,
      error,
      errorCanRetry,
      hostInfo,
      reconnecting,
      reconnectAttempts,
      connectWithConfig,
      disconnect,
      retryConnection,
    ]
  );

  return (
    <ConnectionContext.Provider value={value}>
      {children}
    </ConnectionContext.Provider>
  );
}
