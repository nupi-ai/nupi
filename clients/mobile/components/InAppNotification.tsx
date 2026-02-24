import { useEffect, useRef, useState } from "react";
import { Animated, Pressable, StyleSheet, View as RNView } from "react-native";
import { router } from "expo-router";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import FontAwesome from "@expo/vector-icons/FontAwesome";

import { Text } from "@/components/Themed";
import { useThemeColors } from "@/components/useColorScheme";
import Colors from "@/constants/designTokens";
import { useNotifications } from "@/lib/NotificationContext";
import { isValidNotificationUrl } from "@/lib/notifications";

function eventIcon(eventType?: string): keyof typeof FontAwesome.glyphMap {
  switch (eventType) {
    case "TASK_COMPLETED":
      return "check-circle";
    case "INPUT_NEEDED":
      return "question-circle";
    case "ERROR":
      return "exclamation-circle";
    default:
      return "bell";
  }
}

function eventColor(eventType: string | undefined, colors: (typeof Colors)["light"]): string {
  switch (eventType) {
    case "TASK_COMPLETED":
      return colors.success;
    case "ERROR":
      return colors.danger;
    case "INPUT_NEEDED":
      return colors.warning;
    default:
      return colors.tint;
  }
}

export default function InAppNotificationBanner() {
  const colors = useThemeColors();
  const insets = useSafeAreaInsets();
  const { currentNotification, dismiss } = useNotifications();
  const slideAnim = useRef(new Animated.Value(-100)).current;
  const pressedRef = useRef(false);
  // M7 fix: keep component mounted during slide-out animation.
  const [visible, setVisible] = useState(false);
  // H2/R5 fix: preserve last notification content for display during slide-out
  // so the banner doesn't flash empty text while animating away.
  const lastNotifRef = useRef(currentNotification);

  // L1 fix (Review 14): split into two effects to break the dependency cycle
  // where the effect depends on `visible` state that it also sets.
  // Effect 1: slide in when a notification arrives.
  useEffect(() => {
    if (currentNotification) {
      lastNotifRef.current = currentNotification;
      setVisible(true);
      pressedRef.current = false;
      // L2 fix: wait for stopAnimation to complete before resetting value
      // and starting the slide-in spring, preventing visual glitches.
      slideAnim.stopAnimation(() => {
        slideAnim.setValue(-100);
        Animated.spring(slideAnim, {
          toValue: 0,
          useNativeDriver: true,
          tension: 80,
          friction: 12,
        }).start();
      });
    }
  }, [currentNotification, slideAnim]);

  // Reset tap debounce when notification is dismissed so the next
  // notification's tap handler is not blocked (H3 review fix).
  useEffect(() => {
    if (!currentNotification) {
      pressedRef.current = false;
    }
  }, [currentNotification]);

  // Effect 2: slide out when notification is dismissed (becomes null).
  const prevNotifRef = useRef(currentNotification);
  useEffect(() => {
    const wasShowing = prevNotifRef.current !== null;
    prevNotifRef.current = currentNotification;
    if (currentNotification === null && wasShowing && visible) {
      Animated.timing(slideAnim, {
        toValue: -100,
        duration: 200,
        useNativeDriver: true,
      }).start(({ finished }) => {
        if (finished) setVisible(false);
      });
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps -- slideAnim is a stable Animated.Value ref
  }, [currentNotification, visible]);

  if (!visible && !currentNotification) return null;

  // H2/R5 fix: use lastNotifRef during slide-out so text remains visible.
  const notif = currentNotification ?? lastNotifRef.current;
  const iconName = eventIcon(notif?.eventType);
  const iconColor = eventColor(notif?.eventType, colors);

  const handlePress = () => {
    // Debounce: prevent duplicate navigation from rapid taps.
    // Reset occurs in Effect 1 (line ~58) when the next notification arrives,
    // and in dismiss() below as defense-in-depth.
    if (pressedRef.current) return;
    pressedRef.current = true;
    dismiss();
    if (notif?.url && isValidNotificationUrl(notif.url)) {
      try {
        // URL validated by isValidNotificationUrl(); cast matches app/session/[id].tsx route.
        router.push(notif.url as `/session/${string}`);
      } catch (err) {
        console.warn("[Notifications] Banner navigate failed:", notif.url, err);
      }
    }
  };

  return (
    <Animated.View
      style={[
        styles.container,
        {
          backgroundColor: colors.surface,
          // M6 fix (Review 13): increased offset for Dynamic Island clearance
          // on newer iPhones (14 Pro+). insets.top accounts for static safe area
          // but not Dynamic Island expanded states (calls, Live Activities).
          top: insets.top + 16,
          transform: [{ translateY: slideAnim }],
        },
      ]}
    >
      <Pressable
        style={styles.innerContainer}
        onPress={handlePress}
        accessibilityRole="alert"
        accessibilityLabel={`${notif?.title ?? "Notification"}: ${notif?.body ?? ""}`}
      >
        <RNView style={styles.iconContainer}>
          <FontAwesome name={iconName} size={20} color={iconColor} />
        </RNView>
        <RNView style={styles.textContainer}>
          <Text style={[styles.title, { color: colors.text }]} numberOfLines={1}>
            {notif?.title ?? ""}
          </Text>
          <Text
            style={[styles.body, { color: colors.text, opacity: 0.7 }]}
            numberOfLines={2}
          >
            {notif?.body ?? ""}
          </Text>
        </RNView>
        <Pressable
          onPress={dismiss}
          style={styles.dismissButton}
          accessibilityRole="button"
          accessibilityLabel="Dismiss notification"
          hitSlop={8}
        >
          <FontAwesome name="times" size={16} color={colors.text} style={{ opacity: 0.5 }} />
        </Pressable>
      </Pressable>
    </Animated.View>
  );
}

const styles = StyleSheet.create({
  container: {
    position: "absolute",
    left: 12,
    right: 12,
    borderRadius: 12,
    shadowColor: "#000",
    shadowOffset: { width: 0, height: 2 },
    shadowOpacity: 0.25,
    shadowRadius: 4,
    elevation: 5,
    zIndex: 9999,
  },
  innerContainer: {
    flexDirection: "row",
    alignItems: "center",
    paddingHorizontal: 14,
    paddingVertical: 12,
  },
  iconContainer: {
    marginRight: 10,
  },
  textContainer: {
    flex: 1,
  },
  title: {
    fontSize: 14,
    fontWeight: "600",
    marginBottom: 2,
  },
  body: {
    fontSize: 13,
  },
  dismissButton: {
    marginLeft: 8,
    padding: 4,
  },
});
