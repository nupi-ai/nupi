import { StyleSheet, Pressable, View as RNView } from "react-native";

import Colors from "@/constants/Colors";
import { useColorScheme } from "@/components/useColorScheme";
import { Text, View } from "@/components/Themed";

export default function HomeScreen() {
  const colorScheme = useColorScheme() ?? "light";
  const colors = Colors[colorScheme];

  return (
    <View style={styles.container}>
      <Text style={styles.logo}>NUPI</Text>
      <Text style={styles.subtitle}>Mobile Terminal</Text>

      <View
        style={styles.statusContainer}
        lightColor={Colors.light.surface}
        darkColor={Colors.dark.surface}
      >
        <RNView style={[styles.statusDot, { backgroundColor: colors.danger }]} />
        <Text style={styles.statusText}>Not connected</Text>
      </View>

      <Pressable
        style={({ pressed }) => [
          styles.scanButton,
          { backgroundColor: colors.tint, opacity: pressed ? 0.7 : 1 },
        ]}
        accessibilityRole="button"
        accessibilityLabel="Scan QR to Connect"
        accessibilityHint="Opens camera to scan a QR code for pairing with nupid"
      >
        <Text style={styles.scanButtonText}>Scan QR to Connect</Text>
      </Pressable>
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
});
