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
function createVersionedCache<T>(initialValue: T | undefined = undefined) {
  let cache = initialValue;
  let version = 0;

  return {
    async getOrRead(read: () => Promise<T>, onError: () => T): Promise<T> {
      if (cache !== undefined) return cache;
      const vBefore = version;
      try {
        const value = await read();
        // Only update cache if no save/clear occurred while reading.
        if (version === vBefore) cache = value;
        // Use !== undefined (not ??) so explicit null cache values from clear
        // are not bypassed by stale reads when operations race.
        return cache !== undefined ? cache : value;
      } catch {
        return onError();
      }
    },
    set(value: T): void {
      version++;
      cache = value;
    },
    clear(value: T | undefined): void {
      version++;
      cache = value;
    },
  };
}

const tokenCache = createVersionedCache<string | null>();
const languageCache = createVersionedCache<string | null>();

export type StoredConnectionConfig = Pick<
  ConnectionConfig,
  "host" | "port" | "tls"
>;

export async function saveToken(token: string): Promise<void> {
  tokenCache.set(token);
  await SecureStore.setItemAsync(TOKEN_KEY, token);
}

export async function getToken(): Promise<string | null> {
  return tokenCache.getOrRead(() => SecureStore.getItemAsync(TOKEN_KEY), () => null);
}

export async function clearToken(): Promise<void> {
  tokenCache.clear(null);
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
  languageCache.set(lang);
  await SecureStore.setItemAsync(LANGUAGE_KEY, lang);
}

export async function getLanguage(): Promise<string | null> {
  return languageCache.getOrRead(() => SecureStore.getItemAsync(LANGUAGE_KEY), () => null);
}

export async function clearLanguage(): Promise<void> {
  languageCache.clear(null);
  try {
    await SecureStore.deleteItemAsync(LANGUAGE_KEY);
  } catch {
    // May not exist — ignore
  }
}

// Track whether notification permission was prompted during first voice command.
const NOTIF_PROMPTED_KEY = "nupi_notif_prompted";

export async function wasNotificationPermissionPrompted(): Promise<boolean> {
  try {
    return (await SecureStore.getItemAsync(NOTIF_PROMPTED_KEY)) === "1";
  } catch {
    return false;
  }
}

export async function markNotificationPermissionPrompted(): Promise<void> {
  try {
    await SecureStore.setItemAsync(NOTIF_PROMPTED_KEY, "1");
  } catch {
    // Best-effort persist.
  }
}

// Notification preference keys — default: all enabled.
const NOTIF_COMPLETION_KEY = "nupi_notif_completion";
const NOTIF_INPUT_NEEDED_KEY = "nupi_notif_input_needed";
const NOTIF_ERROR_KEY = "nupi_notif_error";

export interface NotificationPreferences {
  completion: boolean;
  inputNeeded: boolean;
  error: boolean;
}

const DEFAULT_NOTIFICATION_PREFS: NotificationPreferences = {
  completion: true,
  inputNeeded: true,
  error: true,
};

const notifPrefsCache = createVersionedCache<NotificationPreferences>();

export async function saveNotificationPreferences(
  prefs: NotificationPreferences
): Promise<void> {
  notifPrefsCache.set(prefs);
  await Promise.all([
    SecureStore.setItemAsync(NOTIF_COMPLETION_KEY, prefs.completion ? "1" : "0"),
    SecureStore.setItemAsync(NOTIF_INPUT_NEEDED_KEY, prefs.inputNeeded ? "1" : "0"),
    SecureStore.setItemAsync(NOTIF_ERROR_KEY, prefs.error ? "1" : "0"),
  ]);
}

export async function getNotificationPreferences(): Promise<NotificationPreferences> {
  return notifPrefsCache.getOrRead(async () => {
    const [comp, input, err] = await Promise.all([
      SecureStore.getItemAsync(NOTIF_COMPLETION_KEY),
      SecureStore.getItemAsync(NOTIF_INPUT_NEEDED_KEY),
      SecureStore.getItemAsync(NOTIF_ERROR_KEY),
    ]);
    return {
      completion: comp !== "0",
      inputNeeded: input !== "0",
      error: err !== "0",
    };
  }, () => DEFAULT_NOTIFICATION_PREFS);
}

export async function clearNotificationPreferences(): Promise<void> {
  notifPrefsCache.clear(undefined);
  try {
    await Promise.all([
      SecureStore.deleteItemAsync(NOTIF_COMPLETION_KEY),
      SecureStore.deleteItemAsync(NOTIF_INPUT_NEEDED_KEY),
      SecureStore.deleteItemAsync(NOTIF_ERROR_KEY),
    ]);
  } catch {
    // May not exist — ignore
  }
}

// TTS audio playback preference — default: enabled.
const TTS_ENABLED_KEY = "nupi_tts_enabled";

const ttsEnabledCache = createVersionedCache<boolean>();

// Cross-component listener for TTS preference changes.
// AudioPlaybackContext subscribes so toggling TTS in Settings takes effect
// on the active session without requiring re-mount.
type TtsChangeListener = (enabled: boolean) => void;
const ttsChangeListeners = new Set<TtsChangeListener>();

/** Subscribe to TTS preference changes. Returns unsubscribe function. */
export function addTtsChangeListener(listener: TtsChangeListener): () => void {
  ttsChangeListeners.add(listener);
  return () => { ttsChangeListeners.delete(listener); };
}

export async function saveTtsEnabled(enabled: boolean): Promise<void> {
  await SecureStore.setItemAsync(TTS_ENABLED_KEY, enabled ? "1" : "0");
  // Update cache and notify listeners AFTER successful write to prevent
  // cache/storage divergence when SecureStore write fails.
  ttsEnabledCache.set(enabled);
  ttsChangeListeners.forEach(fn => fn(enabled));
}

export async function getTtsEnabled(): Promise<boolean> {
  return ttsEnabledCache.getOrRead(async () => {
    const val = await SecureStore.getItemAsync(TTS_ENABLED_KEY);
    return val !== "0"; // default: true
  }, () => true);
}

export async function clearAll(): Promise<void> {
  ttsEnabledCache.clear(undefined);
  await Promise.all([
    clearToken(),
    clearConnectionConfig(),
    clearLanguage(),
    clearNotificationPreferences(),
    SecureStore.deleteItemAsync(NOTIF_PROMPTED_KEY).catch(() => {}),
    SecureStore.deleteItemAsync(TTS_ENABLED_KEY).catch(() => {}),
  ]);
}
