import * as SecureStore from "expo-secure-store";

import type { ConnectionConfig } from "./connect";

export const TOKEN_KEY = "nupi_auth_token";
const CONFIG_KEY = "nupi_connection_config";

export type StoredConnectionConfig = Pick<
  ConnectionConfig,
  "host" | "port" | "tls"
>;

export async function saveToken(token: string): Promise<void> {
  await SecureStore.setItemAsync(TOKEN_KEY, token);
}

export async function getToken(): Promise<string | null> {
  try {
    return await SecureStore.getItemAsync(TOKEN_KEY);
  } catch {
    return null;
  }
}

export async function clearToken(): Promise<void> {
  try {
    await SecureStore.deleteItemAsync(TOKEN_KEY);
  } catch {
    // May not exist — ignore
  }
}

export async function saveConnectionConfig(
  config: StoredConnectionConfig
): Promise<void> {
  await SecureStore.setItemAsync(CONFIG_KEY, JSON.stringify(config));
}

export async function getConnectionConfig(): Promise<StoredConnectionConfig | null> {
  try {
    const raw = await SecureStore.getItemAsync(CONFIG_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (
      typeof parsed.host !== "string" ||
      typeof parsed.port !== "number" ||
      typeof parsed.tls !== "boolean"
    ) {
      return null;
    }
    return parsed as StoredConnectionConfig;
  } catch {
    return null;
  }
}

export async function clearConnectionConfig(): Promise<void> {
  try {
    await SecureStore.deleteItemAsync(CONFIG_KEY);
  } catch {
    // May not exist — ignore
  }
}

export async function clearAll(): Promise<void> {
  await clearToken();
  await clearConnectionConfig();
}
