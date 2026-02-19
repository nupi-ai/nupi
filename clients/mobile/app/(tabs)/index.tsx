import { StyleSheet, Pressable, View as RNView } from "react-native";
import { router } from "expo-router";

import Colors from "@/constants/Colors";
import { useColorScheme } from "@/components/useColorScheme";
import { Text, View } from "@/components/Themed";
import { useConnection } from "@/lib/ConnectionContext";

export default function HomeScreen() {
  const colorScheme = useColorScheme() ?? "light";
  const colors = Colors[colorScheme];
  const connection = useConnection();

  const statusColor =
    connection.status === "connected"
      ? colors.success
      : connection.status === "connecting"
        ? colors.warning
        : colors.danger;

  const statusLabel =
    connection.status === "connected"
      ? `Connected to ${connection.hostInfo}`
      : connection.status === "connecting"
        ? "Connecting\u2026"
        : "Not connected";

  const buttonLabel =
    connection.error || connection.status === "disconnected"
      ? connection.error
        ? "Re-scan QR Code"
        : "Scan QR to Connect"
      : null;

  return (
    <View style={styles.container}>
      <Text style={styles.logo}>NUPI</Text>
      <Text style={styles.subtitle}>Mobile Terminal</Text>

      <View
        style={styles.statusContainer}
        lightColor={Colors.light.surface}
        darkColor={Colors.dark.surface}
      >
        <RNView style={[styles.statusDot, { backgroundColor: statusColor }]} />
        <Text style={styles.statusText}>{statusLabel}</Text>
      </View>

      {connection.error && (
        <Text style={[styles.errorText, { color: colors.danger }]}>
          {connection.error}
        </Text>
      )}

      {buttonLabel && (
        <Pressable
          style={({ pressed }) => [
            styles.scanButton,
            { backgroundColor: colors.tint, opacity: pressed ? 0.7 : 1 },
          ]}
          onPress={() => router.push("/scan")}
          accessibilityRole="button"
          accessibilityLabel={buttonLabel}
          accessibilityHint="Opens camera to scan a QR code for pairing with nupid"
        >
          <Text style={styles.scanButtonText}>{buttonLabel}</Text>
        </Pressable>
      )}

      {connection.status === "connected" && (
        <Pressable
          style={({ pressed }) => [
            styles.disconnectButton,
            { borderColor: colors.danger, opacity: pressed ? 0.7 : 1 },
          ]}
          onPress={connection.disconnect}
          accessibilityRole="button"
          accessibilityLabel="Disconnect"
          accessibilityHint="Disconnects from the paired nupid daemon"
        >
          <Text style={[styles.disconnectText, { color: colors.danger }]}>
            Disconnect
          </Text>
        </Pressable>
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
  scanButton: {
    paddingHorizontal: 24,
    paddingVertical: 14,
    borderRadius: 12,
  },
  scanButtonText: {
    color: "#fff",
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
});
