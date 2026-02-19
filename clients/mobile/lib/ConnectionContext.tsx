import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";

import { ConnectError, Code } from "@connectrpc/connect";

import {
  createNupiClientFromConfig,
  type NupiClient,
} from "./connect";
import {
  clearAll,
  getConnectionConfig,
  getToken,
  saveConnectionConfig,
  saveToken,
  type StoredConnectionConfig,
} from "./storage";

type ConnectionStatus = "disconnected" | "connecting" | "connected";

interface ConnectionContextValue {
  status: ConnectionStatus;
  client: NupiClient | null;
  error: string | null;
  hostInfo: string | null;
  connect: (config: StoredConnectionConfig, token: string) => Promise<void>;
  disconnect: () => Promise<void>;
}

const ConnectionContext = createContext<ConnectionContextValue>({
  status: "disconnected",
  client: null,
  error: null,
  hostInfo: null,
  connect: async () => {},
  disconnect: async () => {},
});

export function useConnection() {
  return useContext(ConnectionContext);
}

export function ConnectionProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<ConnectionStatus>("disconnected");
  const [client, setClient] = useState<NupiClient | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [hostInfo, setHostInfo] = useState<string | null>(null);
  const mountedRef = useRef(true);
  const manualConnectRef = useRef(false);

  const connectWithConfig = useCallback(
    async (config: StoredConnectionConfig, token: string) => {
      manualConnectRef.current = true;
      setStatus("connecting");
      setError(null);

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
          setStatus("connected");
        }
      } catch (err) {
        // Verification failed — keep stored credentials so auto-reconnect
        // can retry (pairing codes are one-time use, token is still valid)
        if (mountedRef.current) {
          setStatus("disconnected");
          setError(err instanceof Error ? err.message : "Connection failed");
        }
        throw err;
      } finally {
        manualConnectRef.current = false;
      }
    },
    []
  );

  const disconnect = useCallback(async () => {
    if (mountedRef.current) {
      setClient(null);
      setHostInfo(null);
      setStatus("disconnected");
      setError(null);
    }
    await clearAll();
  }, []);

  // Auto-reconnect on mount using stored credentials
  useEffect(() => {
    mountedRef.current = true;

    (async () => {
      const token = await getToken();
      const config = await getConnectionConfig();

      if (!token || !config) return;

      if (mountedRef.current) setStatus("connecting");

      try {
        const nupiClient = createNupiClientFromConfig(config);
        await nupiClient.daemon.status({});

        if (mountedRef.current && !manualConnectRef.current) {
          setClient(nupiClient);
          setHostInfo(`${config.host}:${config.port}`);
          setStatus("connected");
        }
      } catch (err) {
        if (mountedRef.current && !manualConnectRef.current) {
          if (err instanceof ConnectError && err.code === Code.Unauthenticated) {
            await clearAll();
            setStatus("disconnected");
            setError("Session expired \u2014 please re-pair.");
          } else {
            setStatus("disconnected");
            setError("Connection lost \u2014 nupid unreachable");
          }
        }
      }
    })();

    return () => {
      mountedRef.current = false;
    };
  }, []);

  const value = React.useMemo(
    () => ({
      status,
      client,
      error,
      hostInfo,
      connect: connectWithConfig,
      disconnect,
    }),
    [status, client, error, hostInfo, connectWithConfig, disconnect]
  );

  return (
    <ConnectionContext.Provider value={value}>
      {children}
    </ConnectionContext.Provider>
  );
}
