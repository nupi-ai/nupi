import { StyleSheet } from "react-native";

import { Text, View } from "@/components/Themed";

export default function SessionsScreen() {
  return (
    <View style={styles.container}>
      <Text style={styles.emptyText}>
        No active sessions. Start a session on your desktop.
      </Text>
      <Text style={styles.hintText}>
        Connect to nupid first using the Home tab.
      </Text>
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
  emptyText: {
    fontSize: 16,
    textAlign: "center",
    opacity: 0.7,
    marginBottom: 8,
  },
  hintText: {
    fontSize: 14,
    textAlign: "center",
    opacity: 0.4,
  },
});
