import * as Notifications from "expo-notifications";
import * as Device from "expo-device";
import * as SecureStore from "expo-secure-store";
import { randomUUID } from "expo-crypto";
import Constants from "expo-constants";
import { Platform } from "react-native";

import type { NupiClient } from "./connect";
import { NotificationEventType } from "./gen/auth_pb";
import {
  getNotificationPreferences,
  type NotificationPreferences,
} from "./storage";
import { raceTimeout } from "./raceTimeout";

const RPC_TIMEOUT = 5000;
const INFLIGHT_TIMEOUT_MS = 15000;
let registerInFlight = false;
let registerInFlightSince = 0;
let registerAbortController: AbortController | null = null;

/**
 * Requests notification permissions from the OS.
 * Returns true if permissions were granted.
 */
export async function requestNotificationPermissions(): Promise<boolean> {
  if (!Device.isDevice) {
    return false;
  }

  const { status: existing } = await Notifications.getPermissionsAsync();
  if (existing === "granted") return true;

  const { status } = await Notifications.requestPermissionsAsync();
  return status === "granted";
}

/**
 * Gets the current notification permission status.
 */
export async function getNotificationPermissionStatus(): Promise<string> {
  const { status } = await Notifications.getPermissionsAsync();
  return status;
}

// Anchoring: YOUR_, PLACEHOLDER, TODO, FIXME, CHANGE_ME match as prefixes
// (any string starting with them). <...>, __...__, {{...}} require full match
// ($ anchored) to avoid false positives on legitimate IDs containing these chars.
const PLACEHOLDER_PATTERN =
  /^(YOUR_|PLACEHOLDER|TODO|FIXME|CHANGE_ME|<.*>$|__.*__$|\{\{.*\}\}$)/i;

function isPlaceholderProjectId(value: string): boolean {
  return PLACEHOLDER_PATTERN.test(value);
}

/**
 * Set to true when the EAS projectId is a placeholder. Exported so the
 * settings screen can show a visible "push not configured" indicator
 * instead of silently broken notification toggles.
 */
export let pushNotConfigured = false;

/**
 * Gets the Expo push token for this device.
 * Requires notification permissions to be granted first.
 */
export async function getExpoPushToken(): Promise<string | null> {
  if (!Device.isDevice) return null;

  try {
    const raw =
      Constants.expoConfig?.extra?.eas?.projectId ??
      (Constants as any).easConfig?.projectId;
    // Guard against placeholder values left in app.json.
    const projectId =
      typeof raw === "string" && raw.length > 0 && !isPlaceholderProjectId(raw)
        ? raw
        : undefined;
    if (!projectId) {
      pushNotConfigured = true;
      const msg =
        "[Notifications] EAS projectId is not configured in app.json " +
        "(extra.eas.projectId). Push notifications will NOT work. " +
        "Run `eas project:init` in clients/mobile/ to set the real project ID.";
      console.error(msg);
      if (__DEV__) {
        throw new Error(msg);
      }
      // Production builds degrade gracefully — no push, settings UI shows warning.
      return null;
    }
    const tokenData = await Notifications.getExpoPushTokenAsync({
      projectId,
    });
    return tokenData.data;
  } catch (err) {
    console.warn("[Notifications] Failed to get Expo push token:", err);
    return null;
  }
}

const DEVICE_ID_KEY = "nupi_device_id";
let cachedDeviceId: string | null = null;

/**
 * Gets a stable, unique device identifier for push token registration.
 * Generates a UUID on first call and persists it in SecureStore.
 */
export async function getDeviceId(): Promise<string> {
  if (cachedDeviceId) return cachedDeviceId;

  try {
    const stored = await SecureStore.getItemAsync(DEVICE_ID_KEY);
    if (stored) {
      cachedDeviceId = stored;
      return stored;
    }
  } catch (err) {
    // SecureStore may fail on first access — fall through to generate.
    console.debug("[Notifications] SecureStore read failed, generating new ID:", err);
  }

  const id = generateUUID();
  cachedDeviceId = id;

  // Retry SecureStore write once after a short delay. If both attempts fail,
  // the cached value is used for this session but a new ID will be generated
  // on next app restart — causing phantom device entries in the daemon store.
  let persisted = false;
  for (let attempt = 0; attempt < 2 && !persisted; attempt++) {
    try {
      if (attempt > 0) await new Promise((r) => setTimeout(r, 500));
      await SecureStore.setItemAsync(DEVICE_ID_KEY, id);
      persisted = true;
    } catch (err) {
      console.warn(
        `[Notifications] SecureStore write failed (attempt ${attempt + 1}/2):`,
        err
      );
    }
  }
  if (!persisted) {
    console.error(
      "[Notifications] Could not persist device ID. " +
        "A new ID will be generated on next restart, causing duplicate push token registrations."
    );
  }
  return id;
}

function generateUUID(): string {
  return randomUUID();
}

/**
 * Converts notification preferences to proto-compatible enabled events array.
 */
export function prefsToEnabledEvents(
  prefs: NotificationPreferences
): NotificationEventType[] {
  const events: NotificationEventType[] = [];
  if (prefs.completion)
    events.push(NotificationEventType.TASK_COMPLETED);
  if (prefs.inputNeeded)
    events.push(NotificationEventType.INPUT_NEEDED);
  if (prefs.error) events.push(NotificationEventType.ERROR);
  return events;
}

/**
 * Registers the push token with the daemon.
 * Should be called after successful connection and permission grant.
 *
 * Returns `true` on success, `false` on failure, or `null` if another
 * registration is already in flight (callers should not treat this as error).
 */
export async function registerPushToken(
  client: NupiClient
): Promise<boolean | null> {
  // L5 fix (Review 13): return null (skipped, not failed) on simulator so
  // callers don't trigger the 5s+15s+30s retry loop. Expo push tokens are
  // device-only — retrying on simulator is always futile.
  if (!Device.isDevice) return null;
  // Re-entry guard: prevent concurrent registration RPCs.
  // Returns null (not false) so callers can distinguish "skipped" from "failed".
  // Safety: if the guard has been stuck for >15s (e.g., Fast Refresh killed the
  // previous call mid-flight), reset it to prevent permanent lockout.
  if (registerInFlight) {
    if (Date.now() - registerInFlightSince > INFLIGHT_TIMEOUT_MS) {
      registerInFlight = false;
    } else {
      return null;
    }
  }
  registerInFlight = true;
  registerInFlightSince = Date.now();
  const abort = new AbortController();
  registerAbortController = abort;

  try {
    if (abort.signal.aborted) return null;

    const token = await getExpoPushToken();
    if (!token) return false;

    if (abort.signal.aborted) return null;

    const prefs = await getNotificationPreferences();
    const enabledEvents = prefsToEnabledEvents(prefs);

    // If all preferences are disabled, unregister the stale token so daemon
    // stops sending pushes for previously enabled events.
    if (enabledEvents.length === 0) {
      const ok = await unregisterPushToken(client);
      return ok; // Return actual result so caller can retry on failure.
    }

    if (abort.signal.aborted) return null;

    const deviceId = await getDeviceId();

    await raceTimeout(
      client.auth.registerPushToken({
        token,
        deviceId,
        enabledEvents,
      }),
      RPC_TIMEOUT,
      "registerPushToken timed out"
    );
    return true;
  } catch (err) {
    if (abort.signal.aborted) return null;
    console.warn("[Notifications] registerPushToken failed:", err);
    return false;
  } finally {
    registerInFlight = false;
    if (registerAbortController === abort) {
      registerAbortController = null;
    }
  }
}

/**
 * H1 fix (Review 14): Aborts any in-flight registerPushToken call.
 * Called by disconnect() to prevent stale registration from racing with
 * unregister. Clears the in-flight guard so the next connect can register.
 */
export function cancelInflightRegistration(): void {
  if (registerAbortController) {
    registerAbortController.abort();
    registerAbortController = null;
  }
  registerInFlight = false;
}

/**
 * Unregisters the push token from the daemon.
 * Should be called on disconnect.
 * Returns true on success, false if the RPC failed (best-effort — daemon
 * will clean up stale tokens via DeviceNotRegistered on next push attempt).
 */
export async function unregisterPushToken(
  client: NupiClient
): Promise<boolean> {
  const deviceId = await getDeviceId();
  try {
    await raceTimeout(
      client.auth.unregisterPushToken({ deviceId }),
      RPC_TIMEOUT,
      "unregisterPushToken timed out"
    );
    return true;
  } catch (err) {
    console.warn("[Notifications] unregisterPushToken failed:", err);
    return false;
  }
}

/**
 * C1 fix (Review 13): Unregister using a pre-captured auth token.
 * Used by disconnect() to avoid race with clearAll() which wipes the
 * token cache. The captured token is injected directly into the RPC
 * headers, bypassing the normal auth interceptor.
 */
export async function unregisterPushTokenWithToken(
  client: NupiClient,
  authToken: string
): Promise<boolean> {
  const deviceId = await getDeviceId();
  try {
    await raceTimeout(
      client.auth.unregisterPushToken(
        { deviceId },
        { headers: { Authorization: `Bearer ${authToken}` } }
      ),
      RPC_TIMEOUT,
      "unregisterPushToken timed out"
    );
    return true;
  } catch (err) {
    console.warn("[Notifications] unregisterPushTokenWithToken failed:", err);
    return false;
  }
}

/**
 * Sets up the Android notification channel for session events.
 */
export async function setupNotificationChannel(): Promise<void> {
  if (Platform.OS === "android") {
    await Notifications.setNotificationChannelAsync("nupi-session-events", {
      name: "Session Events",
      importance: Notifications.AndroidImportance.HIGH,
      sound: "default",
      vibrationPattern: [0, 250],
    });
  }
}

/**
 * Validates a push notification deep link URL against an allowlist of known routes.
 * Prevents URL injection from push notification payloads.
 */
const ALLOWED_NOTIFICATION_URL = /^\/session\/[a-zA-Z0-9_-]{8,128}$/;

export function isValidNotificationUrl(url: string): boolean {
  return ALLOWED_NOTIFICATION_URL.test(url);
}
