import { Pressable, StyleSheet, View as RNView } from "react-native";

import type { Session } from "@/lib/gen/sessions_pb";
import Colors from "@/constants/designTokens";
import { useThemeColors } from "@/components/useColorScheme";
import { Text } from "@/components/Themed";
import { formatRelativeTime } from "@/lib/time";

interface SessionItemProps {
  session: Session;
  onPress: () => void;
}

function getStatusColor(
  status: string,
  colors: (typeof Colors)["light"]
): string {
  switch (status) {
    case "running":
      return colors.success;
    case "detached":
      return colors.warning;
    case "stopped":
      return colors.danger;
    default:
      return colors.tint;
  }
}

export function SessionItem({ session, onPress }: SessionItemProps) {
  const colors = useThemeColors();
  const statusColor = getStatusColor(session.status, colors);

  const displayName = session.tool || session.command || "unknown";
  const shortId = session.id.slice(0, 8);
  const timeAgo = formatRelativeTime(session.startTime);

  return (
    <Pressable
      style={({ pressed }) => [
        styles.container,
        { backgroundColor: colors.surface, opacity: pressed ? 0.7 : 1 },
      ]}
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={`Session ${displayName}, status ${session.status}, started ${timeAgo || "unknown time"}`}
      accessibilityHint="Opens terminal view for this session"
    >
      <RNView style={styles.row}>
        <RNView style={styles.left}>
          <RNView style={styles.nameRow}>
            <Text style={styles.name}>{displayName}</Text>
            {session.tool && session.tool !== session.command && (
              <Text style={styles.command}>{session.command}</Text>
            )}
          </RNView>
          <Text style={styles.meta}>
            {shortId}{timeAgo ? ` \u00b7 ${timeAgo}` : ""}
          </Text>
        </RNView>
        <RNView style={styles.right}>
          <RNView style={styles.statusBadge}>
            <RNView
              style={[styles.statusDot, { backgroundColor: statusColor }]}
            />
            <Text style={[styles.statusText, { color: statusColor }]}>
              {session.status}
            </Text>
          </RNView>
        </RNView>
      </RNView>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  container: {
    marginHorizontal: 16,
    marginVertical: 4,
    borderRadius: 12,
    padding: 14,
  },
  row: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-between",
  },
  left: {
    flex: 1,
    marginRight: 12,
  },
  nameRow: {
    flexDirection: "row",
    alignItems: "center",
    marginBottom: 4,
  },
  name: {
    fontSize: 16,
    fontWeight: "600",
  },
  command: {
    fontSize: 13,
    opacity: 0.5,
    marginLeft: 8,
  },
  meta: {
    fontSize: 13,
    opacity: 0.5,
  },
  right: {
    alignItems: "flex-end",
  },
  statusBadge: {
    flexDirection: "row",
    alignItems: "center",
  },
  statusDot: {
    width: 8,
    height: 8,
    borderRadius: 4,
    marginRight: 6,
  },
  statusText: {
    fontSize: 13,
    fontWeight: "500",
  },
});
