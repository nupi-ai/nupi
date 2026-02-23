import { StyleSheet, View as RNView } from "react-native";
import { router } from "expo-router";

import { Button } from "@/components/Button";
import Colors from "@/constants/Colors";
import { useColorScheme } from "@/components/useColorScheme";
import { Text, View } from "@/components/Themed";
import { useConnection } from "@/lib/ConnectionContext";

export default function HomeScreen() {
  const colorScheme = useColorScheme() ?? "light";
  const colors = Colors[colorScheme];
  const connection = useConnection();

  const isReconnecting = connection.reconnecting;

  const statusColor =
    connection.status === "connected"
      ? colors.success
      : isReconnecting
        ? colors.warning
        : connection.status === "connecting"
          ? colors.warning
          : colors.danger;

  const statusLabel =
    connection.status === "connected"
      ? `Connected to ${connection.hostInfo}`
      : isReconnecting
        ? connection.reconnectAttempts > 0
          ? `Reconnecting\u2026 (attempt ${connection.reconnectAttempts}/3)`
          : "Reconnecting\u2026"
        : connection.status === "connecting"
          ? "Connecting\u2026"
          : "Not connected";

  const showScanButton =
    !isReconnecting &&
    (connection.error || connection.status === "disconnected") &&
    connection.status !== "connected";

  const showRetryButton =
    !isReconnecting &&
    connection.error &&
    connection.errorCanRetry &&
    connection.status === "disconnected";

  return (
    <View style={styles.container}>
      <Text style={styles.logo}>NUPI</Text>
      <Text style={styles.subtitle}>Mobile Terminal</Text>

      <View
        style={styles.statusContainer}
        lightColor={Colors.light.surface}
        darkColor={Colors.dark.surface}
        accessible={true}
        accessibilityLabel={statusLabel}
        accessibilityLiveRegion="polite"
      >
        <RNView style={[styles.statusDot, { backgroundColor: statusColor }]} />
        <Text style={styles.statusText}>{statusLabel}</Text>
      </View>

      {connection.error && !isReconnecting && (
        <Text style={[styles.errorText, { color: colors.danger }]}>
          {connection.error}
        </Text>
      )}

      {showRetryButton && (
        <Button
          style={styles.retryButton}
          variant="primary"
          color={colors.tint}
          onPress={connection.retryConnection}
          accessibilityLabel="Retry connection"
          accessibilityHint="Attempts to reconnect to nupid"
          testID="retry-connection-button"
        >
          <Text style={[styles.buttonText, { color: colors.background }]}>Retry</Text>
        </Button>
      )}

      {showScanButton && (
        <Button
          style={showRetryButton ? styles.scanButtonOutline : styles.scanButton}
          variant={showRetryButton ? "outline" : "primary"}
          color={colors.tint}
          onPress={() => router.push("/scan")}
          accessibilityLabel={connection.error ? "Re-scan QR Code" : "Scan QR to Connect"}
          accessibilityHint="Opens camera to scan a QR code for pairing with nupid"
          testID="scan-qr-button"
        >
          <Text
            style={[
              styles.buttonText,
              { color: showRetryButton ? colors.tint : colors.background },
            ]}
          >
            {connection.error ? "Re-scan QR Code" : "Scan QR to Connect"}
          </Text>
        </Button>
      )}

      {showScanButton && connection.error && (
        <Button
          style={styles.settingsButton}
          variant="outline"
          color={colors.tint}
          onPress={() => router.push("/(tabs)/settings")}
          accessibilityLabel="Go to settings"
          accessibilityHint="View connection details and manage pairing"
          testID="home-go-to-settings-button"
        >
          <Text style={[styles.settingsButtonText, { color: colors.tint }]}>
            Go to Settings
          </Text>
        </Button>
      )}

      {connection.status === "connected" && (
        <Button
          style={styles.disconnectButton}
          variant="outline"
          color={colors.danger}
          onPress={connection.disconnect}
          accessibilityLabel="Disconnect"
          accessibilityHint="Disconnects from the paired nupid daemon"
          testID="disconnect-button"
        >
          <Text style={[styles.disconnectText, { color: colors.danger }]}>
            Disconnect
          </Text>
        </Button>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    alignItems: "center",
    justifyContent: "center",
    padding: 20,
  },
  logo: {
    fontSize: 48,
    fontWeight: "bold",
    letterSpacing: 8,
    marginBottom: 8,
  },
  subtitle: {
    fontSize: 16,
    opacity: 0.6,
    marginBottom: 40,
  },
  statusContainer: {
    flexDirection: "row",
    alignItems: "center",
    paddingHorizontal: 16,
    paddingVertical: 10,
    borderRadius: 20,
    marginBottom: 40,
  },
  statusDot: {
    width: 8,
    height: 8,
    borderRadius: 4,
    marginRight: 8,
  },
  statusText: {
    fontSize: 14,
    opacity: 0.7,
  },
  errorText: {
    fontSize: 14,
    textAlign: "center",
    marginBottom: 16,
  },
  retryButton: {
    paddingHorizontal: 24,
    paddingVertical: 14,
    borderRadius: 12,
    marginBottom: 12,
  },
  scanButton: {
    paddingHorizontal: 24,
    paddingVertical: 14,
    borderRadius: 12,
  },
  scanButtonOutline: {
    paddingHorizontal: 24,
    paddingVertical: 14,
    borderRadius: 12,
    borderWidth: 1,
  },
  buttonText: {
    fontSize: 16,
    fontWeight: "600",
  },
  disconnectButton: {
    marginTop: 16,
    paddingHorizontal: 24,
    paddingVertical: 14,
    borderRadius: 12,
    borderWidth: 1,
  },
  disconnectText: {
    fontSize: 16,
    fontWeight: "600",
  },
  settingsButton: {
    marginTop: 12,
    paddingHorizontal: 24,
    paddingVertical: 14,
    borderRadius: 12,
    borderWidth: 1,
  },
  settingsButtonText: {
    fontSize: 16,
    fontWeight: "600",
  },
});
