import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import { AppState, type AppStateStatus } from "react-native";
import * as Notifications from "expo-notifications";
import { router } from "expo-router";

import { useConnection } from "./ConnectionContext";
import {
  registerPushToken,
  unregisterPushToken,
  setupNotificationChannel,
  requestNotificationPermissions,
  getNotificationPermissionStatus,
  isValidNotificationUrl,
} from "./notifications";
import { getNotificationPreferences, type NotificationPreferences } from "./storage";

// M7 fix (Review 13): moved setNotificationHandler call from module scope
// into the NotificationProvider component to avoid side effects on import
// (breaks Fast Refresh and test isolation).
let notificationHandlerSet = false;
function ensureNotificationHandler() {
  if (notificationHandlerSet) return;
  notificationHandlerSet = true;
  Notifications.setNotificationHandler({
    handleNotification: async () => ({
      shouldShowBanner: false,
      shouldShowList: false,
      shouldPlaySound: false,
      shouldSetBadge: false,
    }),
  });
}

export interface InAppNotification {
  id: string;
  title: string;
  body: string;
  sessionId?: string;
  eventType?: string;
  url?: string;
}

interface NotificationContextValue {
  /** Current in-app notification to display (or null). */
  currentNotification: InAppNotification | null;
  /** Dismiss the current in-app notification. */
  dismiss: () => void;
  /** Whether notification permissions are granted. */
  permissionGranted: boolean;
  /** Request notification permissions. */
  requestPermission: () => Promise<boolean>;
  /** Force re-registration on next connection cycle (call after pref change). */
  invalidateRegistration: () => void;
  /** M5 fix (Review 13): Cancel any pending registration retry timer immediately.
   * Called by disconnect() before unregisterPushToken to prevent a pending
   * retry from re-registering after unregister fires. */
  cancelPendingRegistration: () => void;
}

const NotificationCtx = createContext<NotificationContextValue>({
  currentNotification: null,
  dismiss: () => {},
  permissionGranted: false,
  requestPermission: async () => false,
  invalidateRegistration: () => {},
  cancelPendingRegistration: () => {},
});

export function useNotifications() {
  return useContext(NotificationCtx);
}

const AUTO_DISMISS_MS = 5000;
const MAX_HANDLED_RESPONSES = 200;
const HANDLED_RESPONSE_TTL_MS = 5 * 60 * 1000; // 5 minutes

export function NotificationProvider({
  children,
}: {
  children: React.ReactNode;
}) {
  ensureNotificationHandler();
  const { status, client, reconnecting, onBeforeDisconnectRef } = useConnection();
  const [currentNotification, setCurrentNotification] =
    useState<InAppNotification | null>(null);
  const [permissionGranted, setPermissionGranted] = useState(false);
  const [registrationGen, setRegistrationGen] = useState(0);
  const dismissTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const registeredRef = useRef(false);
  const mountedRef = useRef(true);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const retryAttemptRef = useRef(0);
  const refreshRetryRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const handledResponsesRef = useRef(new Map<string, number>());
  // Ref mirrors for values used inside setTimeout callbacks (avoids stale closures).
  const statusRef = useRef(status);
  const clientRef = useRef(client);
  const permGrantedRef = useRef(permissionGranted);
  statusRef.current = status;
  clientRef.current = client;
  permGrantedRef.current = permissionGranted;

  // Check permission status on mount and when returning from background
  // (user may have toggled permissions in OS Settings).
  useEffect(() => {
    mountedRef.current = true;

    const checkPermission = async () => {
      try {
        const perm = await getNotificationPermissionStatus();
        if (mountedRef.current) setPermissionGranted(perm === "granted");
      } catch (err) {
        console.warn("[Notifications] Failed to check permission status:", err);
        if (mountedRef.current) setPermissionGranted(false);
      }
    };

    checkPermission();
    setupNotificationChannel().catch((err) => {
      console.warn("[Notifications] Failed to setup notification channel:", err);
    });

    const handleAppStateChange = (nextState: AppStateStatus) => {
      if (nextState === "active") {
        checkPermission();
      }
    };
    const sub = AppState.addEventListener("change", handleAppStateChange);

    return () => {
      mountedRef.current = false;
      sub.remove();
    };
  }, []);

  // Register push token on connect, unregister on manual disconnect.
  useEffect(() => {
    const RETRY_DELAYS = [5000, 15000, 30000];

    if (status === "connected" && client && permissionGranted && !registeredRef.current) {
      const attemptRegister = (attempt: number) => {
        registerPushToken(client)
          .then((result) => {
            if (result === true) {
              registeredRef.current = true;
              retryAttemptRef.current = 0;
            } else if (result === false && attempt < RETRY_DELAYS.length) {
              // Retry on actual failure (false).
              retryTimerRef.current = setTimeout(() => {
                if (mountedRef.current && !registeredRef.current) {
                  attemptRegister(attempt + 1);
                }
              }, RETRY_DELAYS[attempt]);
            } else if (result === null && attempt < RETRY_DELAYS.length) {
              // H4 fix (Review 14): null means "skipped" (in-flight guard or
              // simulator). Schedule a retry with shorter delay — the in-flight
              // call will likely complete before the retry fires.
              retryTimerRef.current = setTimeout(() => {
                if (mountedRef.current && !registeredRef.current) {
                  attemptRegister(attempt + 1);
                }
              }, 2000);
            }
          })
          .catch((err: unknown) => {
            console.warn("[Notifications] registration attempt failed:", err);
          });
      };
      attemptRegister(retryAttemptRef.current);
    }

    // On disconnect, reset registration state. The actual unregisterPushToken
    // RPC is called in useClientLifecycle.disconnect() before clearAll() wipes
    // the auth token — doing it here would be too late (auth already cleared).
    if (
      status === "disconnected" &&
      registeredRef.current &&
      !reconnecting
    ) {
      registeredRef.current = false;
      retryAttemptRef.current = 0;
      if (retryTimerRef.current) {
        clearTimeout(retryTimerRef.current);
        retryTimerRef.current = null;
      }
    }

    return () => {
      if (retryTimerRef.current) {
        clearTimeout(retryTimerRef.current);
        retryTimerRef.current = null;
      }
    };
  }, [status, client, permissionGranted, reconnecting, registrationGen]);

  // Listen for token refresh — re-register if token changes.
  // Uses refs inside setTimeout to avoid stale closure over status/client.
  useEffect(() => {
    const subscription = Notifications.addPushTokenListener(async () => {
      if (!mountedRef.current) return;
      if (statusRef.current === "connected" && clientRef.current && permGrantedRef.current) {
        const result = await registerPushToken(clientRef.current);
        // Retry once after 2s if skipped due to in-flight guard.
        if (result === null && mountedRef.current) {
          refreshRetryRef.current = setTimeout(() => {
            refreshRetryRef.current = null;
            if (mountedRef.current && statusRef.current === "connected" && clientRef.current) {
              registerPushToken(clientRef.current).catch(() => {});
            }
          }, 2000);
        }
      }
    });
    return () => {
      subscription.remove();
      if (refreshRetryRef.current) {
        clearTimeout(refreshRetryRef.current);
        refreshRetryRef.current = null;
      }
    };
  }, []);

  // Foreground notification received — show in-app banner.
  useEffect(() => {
    const subscription = Notifications.addNotificationReceivedListener(
      async (notification) => {
        if (!mountedRef.current) return;

        const prefs = await getNotificationPreferences();
        if (!mountedRef.current) return;
        const data = notification.request.content.data as Record<string, unknown> | undefined;
        // Type-guard: only use string values from external push payload.
        const eventType = typeof data?.eventType === "string" ? data.eventType : undefined;
        const sessionId = typeof data?.sessionId === "string" ? data.sessionId : undefined;
        const rawUrl = typeof data?.url === "string" ? data.url : undefined;

        // Check local preferences before showing.
        if (eventType && !shouldShowNotification(eventType, prefs)) return;

        // C1 fix: validate URL before including in InAppNotification.
        const validatedUrl = rawUrl && isValidNotificationUrl(rawUrl) ? rawUrl : undefined;

        // M8 fix (Review 14): truncate title and body from external push payload
        // to prevent oversized strings from consuming memory or exploiting rendering.
        const rawTitle = notification.request.content.title ?? "Nupi";
        const rawBody = notification.request.content.body ?? "";
        const MAX_TITLE_LEN = 128;
        const MAX_BODY_LEN = 1024;

        const inApp: InAppNotification = {
          id: notification.request.identifier,
          title: rawTitle.length > MAX_TITLE_LEN ? rawTitle.slice(0, MAX_TITLE_LEN) + "\u2026" : rawTitle,
          body: rawBody.length > MAX_BODY_LEN ? rawBody.slice(0, MAX_BODY_LEN) + "\u2026" : rawBody,
          sessionId,
          eventType,
          url: validatedUrl,
        };

        setCurrentNotification(inApp);

        // Auto-dismiss after 5s.
        if (dismissTimerRef.current) clearTimeout(dismissTimerRef.current);
        dismissTimerRef.current = setTimeout(() => {
          if (mountedRef.current) setCurrentNotification(null);
          dismissTimerRef.current = null;
        }, AUTO_DISMISS_MS);
      }
    );
    return () => {
      subscription.remove();
      if (dismissTimerRef.current) {
        clearTimeout(dismissTimerRef.current);
        dismissTimerRef.current = null;
      }
    };
  }, []);

  // Deep link from notification tap (both foreground and background).
  useEffect(() => {
    const subscription = Notifications.addNotificationResponseReceivedListener(
      (response) => {
        const responseId = response.notification.request.identifier;
        if (handledResponsesRef.current.has(responseId)) return;
        trackHandledResponse(handledResponsesRef, responseId);

        const data = response.notification.request.content.data as
          | Record<string, unknown>
          | undefined;
        const url = typeof data?.url === "string" ? data.url : undefined;
        if (url && isValidNotificationUrl(url)) {
          try {
            // URL validated against /session/[id] pattern; cast matches app/session/[id].tsx route.
            router.push(url as `/session/${string}`);
          } catch (err) {
            console.warn("[Notifications] Failed to navigate from tap:", url, err);
          }
        }
      }
    );
    return () => subscription.remove();
  }, []);

  // Cold start: handle notification that launched the app.
  // Uses retry with exponential backoff: Expo Router needs time to mount the
  // navigation tree after a cold launch. Delays are calibrated to cover:
  //   200ms — fast devices (iPhone 13+, Pixel 6+)
  //   500ms — mid-range devices
  //  1200ms — slow devices / heavy app startup
  // Total worst-case wait: ~2050ms, well within acceptable cold-start UX.
  useEffect(() => {
    let cancelled = false;
    const timerIds: ReturnType<typeof setTimeout>[] = [];
    const COLD_START_DELAYS = [200, 500, 1200];
    // Delay before verifying router.canGoBack(). Must be long enough for
    // the navigation transition to register (150ms covers iOS and Android
    // native navigation animation start latency).
    const VERIFY_DELAY_MS = 150;

    (async () => {
      let lastResponse;
      try {
        lastResponse = await Notifications.getLastNotificationResponseAsync();
      } catch (err) {
        console.warn("[Notifications] Cold start getLastNotificationResponse failed:", err);
        return;
      }
      if (!lastResponse || cancelled) return;

      const responseId = lastResponse.notification.request.identifier;
      if (handledResponsesRef.current.has(responseId)) return;
      trackHandledResponse(handledResponsesRef, responseId);

      const data = lastResponse.notification.request.content.data as
        | Record<string, unknown>
        | undefined;
      const url = typeof data?.url === "string" ? data.url : undefined;
      if (url && isValidNotificationUrl(url)) {
        const attemptNavigate = (attempt: number) => {
          if (cancelled) return;
          try {
            router.push(url as `/session/${string}`);
            const checkTid = setTimeout(() => {
              if (!cancelled && !router.canGoBack() && attempt < COLD_START_DELAYS.length - 1) {
                const tid = setTimeout(
                  () => attemptNavigate(attempt + 1),
                  COLD_START_DELAYS[attempt + 1]!
                );
                timerIds.push(tid);
              } else {
                cancelled = true;
              }
            }, VERIFY_DELAY_MS);
            timerIds.push(checkTid);
          } catch (err) {
            console.warn("[Notifications] Cold start navigate attempt failed:", url, err);
            if (!cancelled && attempt < COLD_START_DELAYS.length - 1) {
              const tid = setTimeout(
                () => attemptNavigate(attempt + 1),
                COLD_START_DELAYS[attempt + 1]!
              );
              timerIds.push(tid);
            }
          }
        };
        const firstTid = setTimeout(() => attemptNavigate(0), COLD_START_DELAYS[0]!);
        timerIds.push(firstTid);
      }
    })();
    return () => {
      cancelled = true;
      timerIds.forEach(clearTimeout);
    };
  }, []);

  const dismiss = useCallback(() => {
    setCurrentNotification(null);
    if (dismissTimerRef.current) {
      clearTimeout(dismissTimerRef.current);
      dismissTimerRef.current = null;
    }
  }, []);

  const requestPermission = useCallback(async () => {
    const granted = await requestNotificationPermissions();
    if (mountedRef.current) setPermissionGranted(granted);
    // Don't call registerPushToken here — setPermissionGranted(true) will
    // trigger the useEffect above which handles registration and correctly
    // sets registeredRef on success.
    return granted;
  }, []);

  // H6 fix: Allow settings screen to invalidate registration so the context
  // re-registers with updated preferences on the next effect cycle.
  // H1/R5 fix: Bump registrationGen to trigger the useEffect (refs alone
  // don't cause re-renders, so the effect wouldn't re-run).
  const invalidateRegistration = useCallback(() => {
    registeredRef.current = false;
    retryAttemptRef.current = 0;
    setRegistrationGen((g) => g + 1);
  }, []);

  // M5 fix (Review 13): synchronously cancel pending retry so disconnect()
  // can call this before firing unregisterPushToken, preventing a race where
  // the retry re-registers after unregister.
  const cancelPendingRegistration = useCallback(() => {
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
    registeredRef.current = false;
    retryAttemptRef.current = 0;
  }, []);

  // Wire cancelPendingRegistration into ConnectionContext's onBeforeDisconnectRef
  // so disconnect() can call it synchronously before unregisterPushToken RPC.
  useEffect(() => {
    onBeforeDisconnectRef.current = cancelPendingRegistration;
    return () => {
      onBeforeDisconnectRef.current = null;
    };
  }, [onBeforeDisconnectRef, cancelPendingRegistration]);

  const value = React.useMemo(
    () => ({
      currentNotification,
      dismiss,
      permissionGranted,
      requestPermission,
      invalidateRegistration,
      cancelPendingRegistration,
    }),
    [currentNotification, dismiss, permissionGranted, requestPermission, invalidateRegistration, cancelPendingRegistration]
  );

  return (
    <NotificationCtx.Provider value={value}>
      {children}
    </NotificationCtx.Provider>
  );
}

function trackHandledResponse(
  ref: React.MutableRefObject<Map<string, number>>,
  responseId: string
) {
  const now = Date.now();
  // TTL eviction: remove entries older than 5 minutes to prevent unbounded growth.
  if (ref.current.size >= MAX_HANDLED_RESPONSES) {
    const keysToRemove: string[] = [];
    for (const [key, ts] of ref.current) {
      if (now - ts > HANDLED_RESPONSE_TTL_MS) {
        keysToRemove.push(key);
      }
    }
    // If TTL didn't free enough, fall back to FIFO eviction of oldest half.
    if (keysToRemove.length === 0) {
      const it = ref.current.keys();
      const toRemove = Math.floor(MAX_HANDLED_RESPONSES / 2);
      for (let i = 0; i < toRemove; i++) {
        const next = it.next();
        if (next.done) break;
        keysToRemove.push(next.value);
      }
    }
    keysToRemove.forEach((k) => ref.current.delete(k));
  }
  ref.current.set(responseId, now);
}

function shouldShowNotification(
  eventType: string,
  prefs: NotificationPreferences
): boolean {
  switch (eventType) {
    case "TASK_COMPLETED":
      return prefs.completion;
    case "INPUT_NEEDED":
      return prefs.inputNeeded;
    case "ERROR":
      return prefs.error;
    default:
      // H2 fix (Review 14): unknown event types are NOT shown. Returning true
      // would bypass user preferences (user disables all → unknown type still
      // shows). Forward-compatibility must be opt-in via explicit mapping, not
      // a default-allow that creates a spam/privacy vector.
      console.debug("[Notifications] Unknown event type, suppressing:", eventType);
      return false;
  }
}
