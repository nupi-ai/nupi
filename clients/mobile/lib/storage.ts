import * as SecureStore from "expo-secure-store";

import type { ConnectionConfig } from "./connect";

export const TOKEN_KEY = "nupi_auth_token";
const CONFIG_KEY = "nupi_connection_config";
export const LANGUAGE_KEY = "nupi_language_preference";

// M2 fix: in-memory cache for values read on every RPC call (auth interceptor,
// language interceptor). Avoids repeated SecureStore disk reads during fast
// polling (500ms intervals → 4 reads/sec without cache).
// Version counters prevent stale SecureStore reads from overwriting newer cache
// values when a save races with an in-flight get.
let cachedToken: string | null | undefined;
let cachedLanguage: string | null | undefined;
let tokenVersion = 0;
let languageVersion = 0;

export type StoredConnectionConfig = Pick<
  ConnectionConfig,
  "host" | "port" | "tls"
>;

export async function saveToken(token: string): Promise<void> {
  tokenVersion++;
  cachedToken = token;
  await SecureStore.setItemAsync(TOKEN_KEY, token);
}

export async function getToken(): Promise<string | null> {
  if (cachedToken !== undefined) return cachedToken;
  const vBefore = tokenVersion;
  try {
    const val = await SecureStore.getItemAsync(TOKEN_KEY);
    // Only update cache if no save occurred while we were reading.
    if (tokenVersion === vBefore) cachedToken = val;
    // Use !== undefined (not ??) so that null from clearToken() isn't
    // bypassed by a stale disk read when the two race.
    return cachedToken !== undefined ? cachedToken : val;
  } catch {
    return null;
  }
}

export async function clearToken(): Promise<void> {
  tokenVersion++;
  cachedToken = null;
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

export async function saveLanguage(lang: string): Promise<void> {
  if (!lang || lang.length > 35) {
    throw new Error("Invalid language code");
  }
  languageVersion++;
  cachedLanguage = lang;
  await SecureStore.setItemAsync(LANGUAGE_KEY, lang);
}

export async function getLanguage(): Promise<string | null> {
  if (cachedLanguage !== undefined) return cachedLanguage;
  const vBefore = languageVersion;
  try {
    const val = await SecureStore.getItemAsync(LANGUAGE_KEY);
    // Only update cache if no save occurred while we were reading.
    if (languageVersion === vBefore) cachedLanguage = val;
    // Use !== undefined (not ??) so that null from clearLanguage() isn't
    // bypassed by a stale disk read when the two race.
    return cachedLanguage !== undefined ? cachedLanguage : val;
  } catch {
    return null;
  }
}

export async function clearLanguage(): Promise<void> {
  languageVersion++;
  cachedLanguage = null;
  try {
    await SecureStore.deleteItemAsync(LANGUAGE_KEY);
  } catch {
    // May not exist — ignore
  }
}

export async function clearAll(): Promise<void> {
  await Promise.all([clearToken(), clearConnectionConfig(), clearLanguage()]);
}
