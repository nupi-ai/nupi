import { useCallback, useEffect, useRef, useState } from "react";
import { RefreshControl, ScrollView, StyleSheet, Switch, View as RNView } from "react-native";
import { router } from "expo-router";
import Constants from "expo-constants";

import { Button } from "@/components/Button";
import Colors from "@/constants/designTokens";
import { useThemeColors } from "@/components/useColorScheme";
import { Text, View } from "@/components/Themed";
import { useConnection } from "@/lib/ConnectionContext";
import { useNotifications } from "@/lib/NotificationContext";
import { mapConnectionError } from "@/lib/errorMessages";
import {
  getNotificationPreferences,
  saveNotificationPreferences,
  getTtsEnabled,
  saveTtsEnabled,
  type NotificationPreferences,
} from "@/lib/storage";

interface DaemonInfo {
  version: string;
  uptimeSec: number;
  tlsEnabled: boolean;
  sessionsCount: number;
}

function formatUptime(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  const hours = Math.floor(seconds / 3600);
  const mins = Math.floor((seconds % 3600) / 60);
  return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`;
}

function InfoRow({
  label,
  value,
  colors,
  accessoryColor,
  isLast,
  accessibilityLiveRegion,
}: {
  label: string;
  value: string;
  colors: (typeof Colors)["light"];
  accessoryColor?: string;
  isLast?: boolean;
  accessibilityLiveRegion?: "polite" | "assertive" | "none";
}) {
  return (
    <RNView
      style={[
        styles.infoRow,
        isLast ? { borderBottomWidth: 0 } : { borderBottomColor: colors.separator },
      ]}
      accessible={true}
      accessibilityLabel={`${label}: ${value}`}
      accessibilityLiveRegion={accessibilityLiveRegion}
    >
      <Text style={[styles.infoLabel, { color: colors.text, opacity: 0.7 }]}>
        {label}
      </Text>
      <RNView style={styles.infoValueRow}>
        <Text style={[styles.infoValue, { color: colors.text }]}>{value}</Text>
        {accessoryColor != null && (
          <RNView
            style={[styles.statusDot, { backgroundColor: accessoryColor }]}
          />
        )}
      </RNView>
    </RNView>
  );
}

export default function SettingsScreen() {
  const colors = useThemeColors();
  const connection = useConnection();

  const mountedRef = useRef(true);
  const fetchGenRef = useRef(0);
  const [daemonInfo, setDaemonInfo] = useState<DaemonInfo | null>(null);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  const { permissionGranted, requestPermission, invalidateRegistration } = useNotifications();
  const [notifPrefs, setNotifPrefs] = useState<NotificationPreferences>({
    completion: true,
    inputNeeded: true,
    error: true,
  });
  const [notifSyncError, setNotifSyncError] = useState<string | null>(null);
  const [ttsEnabled, setTtsEnabled] = useState(true);

  // Load notification preferences and TTS setting on mount.
  useEffect(() => {
    (async () => {
      const [prefs, tts] = await Promise.all([
        getNotificationPreferences(),
        getTtsEnabled(),
      ]);
      if (mountedRef.current) {
        setNotifPrefs(prefs);
        setTtsEnabled(tts);
      }
    })();
  }, []);

  const notifPrefsRef = useRef(notifPrefs);
  notifPrefsRef.current = notifPrefs;

  const updateNotifPref = useCallback(
    async (key: keyof NotificationPreferences, value: boolean) => {
      const prev = notifPrefsRef.current;
      const next = { ...prev, [key]: value };
      setNotifPrefs(next);
      notifPrefsRef.current = next;
      setNotifSyncError(null);
      try {
        await saveNotificationPreferences(next);
        if (connection.status === "connected") {
          invalidateRegistration();
        }
      } catch (err) {
        console.warn("[Settings] Failed to save notification preferences:", err);
        setNotifPrefs(prev);
        notifPrefsRef.current = prev;
        setNotifSyncError("Failed to save preferences");
      }
    },
    [connection.status, invalidateRegistration]
  );

  const handleTtsToggle = useCallback(async (value: boolean) => {
    const prev = ttsEnabled;
    setTtsEnabled(value);
    try {
      await saveTtsEnabled(value);
    } catch (err) {
      console.warn("[Settings] Failed to save TTS preference:", err);
      setTtsEnabled(prev);
    }
  }, [ttsEnabled]);

  const isConnected = connection.status === "connected";
  const isReconnecting = connection.reconnecting;

  const statusColor = isConnected
    ? colors.success
    : isReconnecting || connection.status === "connecting"
      ? colors.warning
      : colors.danger;

  const statusLabel = isConnected
    ? "Connected"
    : isReconnecting
      ? connection.reconnectAttempts > 0
        ? `Reconnecting\u2026 (attempt ${connection.reconnectAttempts}/3)`
        : "Reconnecting\u2026"
      : connection.status === "connecting"
        ? "Connecting\u2026"
        : "Not connected";

  // Parse hostInfo into host and port
  const hostParts = connection.hostInfo?.split(":") ?? [];
  const host =
    hostParts.length > 1
      ? hostParts.slice(0, -1).join(":")
      : connection.hostInfo ?? "\u2014";
  const port =
    hostParts.length > 1
      ? hostParts[hostParts.length - 1]!
      : "\u2014";

  const appVersion = Constants.expoConfig?.version ?? "unknown";

  // Generation counter prevents stale in-flight daemon.status() responses from
  // overwriting state after a connection change or concurrent pull-to-refresh.
  // Each fetch captures the current generation; if another fetch starts before
  // the response arrives, the generation advances and the stale response is
  // discarded.
  const fetchDaemonInfo = useCallback(async () => {
    if (!connection.client) return;
    const gen = ++fetchGenRef.current;
    try {
      const resp = await connection.client.daemon.status({});
      if (mountedRef.current && fetchGenRef.current === gen) {
        setDaemonInfo({
          version: resp.version,
          uptimeSec: Number(resp.uptimeSec),
          tlsEnabled: resp.tlsEnabled,
          sessionsCount: resp.sessionsCount,
        });
        setFetchError(null);
      }
    } catch (err) {
      if (mountedRef.current && fetchGenRef.current === gen) {
        setFetchError(mapConnectionError(err).message);
      }
    }
  }, [connection.client]);

  // Fetch daemon status when connected. On disconnect, bump the generation
  // to invalidate any in-flight request and reset state to placeholders.
  useEffect(() => {
    mountedRef.current = true;

    if (isConnected && connection.client) {
      fetchDaemonInfo();
    } else {
      ++fetchGenRef.current;
      setDaemonInfo(null);
      setFetchError(null);
      setRefreshing(false);
    }

    return () => {
      mountedRef.current = false;
    };
  }, [isConnected, connection.client, fetchDaemonInfo]);

  const handleRefresh = useCallback(async () => {
    if (!isConnected) return;
    setRefreshing(true);
    setFetchError(null);
    await fetchDaemonInfo();
    if (mountedRef.current) setRefreshing(false);
  }, [isConnected, fetchDaemonInfo]);

  return (
    <View style={styles.container}>
      <ScrollView
        contentContainerStyle={styles.scrollContent}
        refreshControl={
          <RefreshControl
            refreshing={refreshing}
            onRefresh={handleRefresh}
            enabled={isConnected}
            tintColor={colors.tint}
          />
        }
      >
        {/* CONNECTION section */}
        <Text
          style={[styles.sectionHeader, { color: colors.text, opacity: 0.5 }]}
          accessibilityRole="header"
        >
          CONNECTION
        </Text>
        <RNView
          style={[styles.section, { backgroundColor: colors.surface }]}
        >
          <InfoRow
            label="Status"
            value={statusLabel}
            colors={colors}
            accessoryColor={statusColor}
            accessibilityLiveRegion="polite"
          />
          <InfoRow label="Host" value={connection.hostInfo ? host : "\u2014"} colors={colors} />
          <InfoRow label="Port" value={connection.hostInfo ? port : "\u2014"} colors={colors} />
          <InfoRow
            label="TLS"
            value={
              isConnected && daemonInfo
                ? daemonInfo.tlsEnabled
                  ? "Enabled"
                  : "Disabled"
                : "\u2014"
            }
            colors={colors}
            isLast
          />
        </RNView>

        {/* DAEMON section */}
        <Text
          style={[styles.sectionHeader, { color: colors.text, opacity: 0.5 }]}
          accessibilityRole="header"
        >
          DAEMON
        </Text>
        <RNView style={[styles.section, { backgroundColor: colors.surface }]}>
          <InfoRow
            label="Version"
            value={daemonInfo ? daemonInfo.version : "\u2014"}
            colors={colors}
          />
          <InfoRow
            label="Uptime"
            value={daemonInfo ? formatUptime(daemonInfo.uptimeSec) : "\u2014"}
            colors={colors}
          />
          <InfoRow
            label="Sessions"
            value={daemonInfo ? String(daemonInfo.sessionsCount) : "\u2014"}
            colors={colors}
            isLast
          />
        </RNView>

        {fetchError && (
          <Text
            style={[styles.fetchErrorText, { color: colors.danger }]}
            accessibilityRole="alert"
            accessibilityLiveRegion="assertive"
          >
            {fetchError}
          </Text>
        )}

        {/* NOTIFICATIONS section */}
        <Text
          style={[styles.sectionHeader, { color: colors.text, opacity: 0.5 }]}
          accessibilityRole="header"
        >
          NOTIFICATIONS
        </Text>
        <RNView style={[styles.section, { backgroundColor: colors.surface }]}>
          <RNView
            style={[
              styles.infoRow,
              { borderBottomColor: colors.separator },
            ]}
          >
            <Text style={[styles.infoLabel, { color: colors.text, opacity: 0.7 }]}>
              Permission
            </Text>
            <RNView style={styles.infoValueRow}>
              <Text style={[styles.infoValue, { color: colors.text }]}>
                {permissionGranted ? "Granted" : "Not granted"}
              </Text>
              <RNView
                style={[
                  styles.statusDot,
                  { backgroundColor: permissionGranted ? colors.success : colors.danger },
                ]}
              />
            </RNView>
          </RNView>
          {!permissionGranted && (
            <Button
              variant="unstyled"
              style={[
                styles.infoRow,
                { borderBottomColor: colors.separator },
              ]}
              onPress={requestPermission}
              accessibilityLabel="Request notification permission"
            >
              <Text style={[styles.infoLabel, { color: colors.tint }]}>
                Request Permission
              </Text>
            </Button>
          )}
          <RNView
            style={[styles.toggleRow, { borderBottomColor: colors.separator }]}
          >
            <Text style={[styles.infoLabel, { color: colors.text, opacity: 0.7 }]}>
              Task completed
            </Text>
            <Switch
              value={notifPrefs.completion}
              onValueChange={(v) => updateNotifPref("completion", v)}
              trackColor={{ true: colors.tint }}
              disabled={!permissionGranted}
              accessibilityLabel="Enable task completion notifications"
              accessibilityHint={!permissionGranted ? "Requires notification permission" : undefined}
              testID="settings-notif-completion"
            />
          </RNView>
          <RNView
            style={[styles.toggleRow, { borderBottomColor: colors.separator }]}
          >
            <Text style={[styles.infoLabel, { color: colors.text, opacity: 0.7 }]}>
              Input needed
            </Text>
            <Switch
              value={notifPrefs.inputNeeded}
              onValueChange={(v) => updateNotifPref("inputNeeded", v)}
              trackColor={{ true: colors.tint }}
              disabled={!permissionGranted}
              accessibilityLabel="Enable input needed notifications"
              accessibilityHint={!permissionGranted ? "Requires notification permission" : undefined}
              testID="settings-notif-input-needed"
            />
          </RNView>
          <RNView
            style={[styles.toggleRow, { borderBottomWidth: 0 }]}
          >
            <Text style={[styles.infoLabel, { color: colors.text, opacity: 0.7 }]}>
              Errors
            </Text>
            <Switch
              value={notifPrefs.error}
              onValueChange={(v) => updateNotifPref("error", v)}
              trackColor={{ true: colors.tint }}
              disabled={!permissionGranted}
              accessibilityLabel="Enable error notifications"
              accessibilityHint={!permissionGranted ? "Requires notification permission" : undefined}
              testID="settings-notif-error"
            />
          </RNView>
        </RNView>

        {notifSyncError && (
          <Text
            style={[styles.fetchErrorText, { color: colors.danger }]}
            accessibilityRole="alert"
            accessibilityLiveRegion="assertive"
          >
            {notifSyncError}
          </Text>
        )}

        {/* AUDIO section */}
        <Text
          style={[styles.sectionHeader, { color: colors.text, opacity: 0.5 }]}
          accessibilityRole="header"
        >
          AUDIO
        </Text>
        <RNView style={[styles.section, { backgroundColor: colors.surface }]}>
          <RNView
            style={[styles.toggleRow, { borderBottomWidth: 0 }]}
          >
            <Text style={[styles.infoLabel, { color: colors.text, opacity: 0.7 }]}>
              TTS Audio Playback
            </Text>
            <Switch
              value={ttsEnabled}
              onValueChange={handleTtsToggle}
              trackColor={{ true: colors.tint }}
              accessibilityLabel="Enable TTS audio playback"
              accessibilityHint="When disabled, AI responses are shown as text only"
              testID="settings-tts-enabled"
            />
          </RNView>
        </RNView>

        {/* ACTIONS section */}
        <Text
          style={[styles.sectionHeader, { color: colors.text, opacity: 0.5 }]}
          accessibilityRole="header"
        >
          ACTIONS
        </Text>
        <RNView style={styles.actionsSection}>
          <Button
            style={styles.actionButton}
            variant="primary"
            color={colors.tint}
            onPress={() => router.push("/scan")}
            accessibilityLabel="Re-pair by scanning QR code"
            accessibilityHint="Opens camera to scan a QR code for pairing with nupid"
            testID="settings-re-pair-button"
          >
            <Text style={[styles.actionButtonText, { color: colors.background }]}>
              Re-pair (scan QR)
            </Text>
          </Button>

          {isConnected && (
            <Button
              style={styles.actionButton}
              variant="danger"
              color={colors.danger}
              onPress={connection.disconnect}
              accessibilityLabel="Disconnect from nupid"
              accessibilityHint="Clears stored credentials and disconnects from the paired daemon"
              testID="settings-disconnect-button"
            >
              <Text style={[styles.actionButtonText, { color: colors.onDanger }]}>
                Disconnect
              </Text>
            </Button>
          )}
        </RNView>

        {/* ABOUT section */}
        <Text
          style={[styles.sectionHeader, { color: colors.text, opacity: 0.5 }]}
          accessibilityRole="header"
        >
          ABOUT
        </Text>
        <RNView style={[styles.section, { backgroundColor: colors.surface }]}>
          <InfoRow label="App Version" value={appVersion} colors={colors} isLast />
        </RNView>
      </ScrollView>
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
  },
  scrollContent: {
    padding: 16,
    paddingBottom: 32,
  },
  sectionHeader: {
    fontSize: 13,
    fontWeight: "600",
    letterSpacing: 0.5,
    marginTop: 24,
    marginBottom: 8,
    marginLeft: 4,
  },
  section: {
    borderRadius: 12,
    overflow: "hidden",
  },
  infoRow: {
    flexDirection: "row",
    justifyContent: "space-between",
    alignItems: "center",
    paddingHorizontal: 16,
    paddingVertical: 13,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  infoLabel: {
    fontSize: 15,
  },
  infoValueRow: {
    flexDirection: "row",
    alignItems: "center",
  },
  infoValue: {
    fontSize: 15,
  },
  statusDot: {
    width: 8,
    height: 8,
    borderRadius: 4,
    marginLeft: 8,
  },
  fetchErrorText: {
    fontSize: 13,
    textAlign: "center",
    marginTop: 8,
    marginBottom: 4,
  },
  actionsSection: {
    gap: 12,
  },
  toggleRow: {
    flexDirection: "row",
    justifyContent: "space-between",
    alignItems: "center",
    paddingHorizontal: 16,
    paddingVertical: 10,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  actionButton: {
    paddingVertical: 14,
    borderRadius: 12,
    alignItems: "center",
  },
  actionButtonText: {
    fontSize: 16,
    fontWeight: "600",
  },
});
