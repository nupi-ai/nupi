import { ActivityIndicator, Pressable, StyleSheet, View as RNView } from "react-native";
import FontAwesome from "@expo/vector-icons/FontAwesome";

import { Text } from "@/components/Themed";
import { useThemeColors } from "@/components/useColorScheme";

interface TranscriptionBubbleProps {
  confirmedText: string;
  pendingText: string;
  isLive: boolean;
  onDismiss: () => void;
}

export function TranscriptionBubble({
  confirmedText,
  pendingText,
  isLive,
  onDismiss,
}: TranscriptionBubbleProps) {
  const colors = useThemeColors("dark");
  const displayText = [confirmedText, pendingText].filter(Boolean).join(" ");

  return (
    <RNView
      style={[
        styles.container,
        { backgroundColor: colors.surface },
      ]}
      accessibilityRole="text"
      accessibilityLabel={
        isLive
          ? `Live transcription: ${displayText}`
          : `Transcribed text: ${displayText}`
      }
      accessibilityLiveRegion={isLive ? undefined : "polite"}
      testID="transcription-bubble"
    >
      {isLive && (
        <ActivityIndicator
          size="small"
          color={colors.danger}
          style={styles.liveIndicator}
          accessibilityLabel="Transcribing in real-time"
        />
      )}
      <Text
        style={[styles.text, { color: colors.text }]}
        numberOfLines={4}
        ellipsizeMode="tail"
      >
        {confirmedText}
        {pendingText ? (
          <Text style={styles.pendingText}>
            {confirmedText ? " " : ""}{pendingText}
          </Text>
        ) : null}
      </Text>
      {!isLive && (
        <Pressable
          onPress={onDismiss}
          style={({ pressed }) => [
            styles.dismissButton,
            { opacity: pressed ? 0.7 : 1 },
          ]}
          accessibilityRole="button"
          accessibilityLabel="Dismiss transcription"
          accessibilityHint="Clears the transcribed text"
          testID="transcription-dismiss"
        >
          <FontAwesome name="times" size={16} color={colors.tint} />
        </Pressable>
      )}
    </RNView>
  );
}

const styles = StyleSheet.create({
  container: {
    marginHorizontal: 12,
    marginBottom: 8,
    padding: 12,
    borderRadius: 10,
    flexDirection: "row",
    alignItems: "flex-start",
    gap: 8,
  },
  liveIndicator: {
    marginTop: 2,
  },
  text: {
    flex: 1,
    fontSize: 14,
    lineHeight: 20,
  },
  pendingText: {
    opacity: 0.5,
  },
  dismissButton: {
    paddingHorizontal: 8,
    paddingVertical: 4,
    minWidth: 44,
    minHeight: 44,
    alignItems: "center",
    justifyContent: "center",
  },
});
