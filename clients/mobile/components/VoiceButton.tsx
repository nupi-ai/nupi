import { useEffect } from "react";
import {
  Pressable,
  StyleSheet,
} from "react-native";
import Animated, {
  useAnimatedStyle,
  useSharedValue,
  withRepeat,
  withSequence,
  withTiming,
  cancelAnimation,
} from "react-native-reanimated";
import FontAwesome from "@expo/vector-icons/FontAwesome";

import { useThemeColors } from "@/components/useColorScheme";
import { type ModelStatus, type RecordingStatus } from "@/lib/VoiceContext";

interface VoiceButtonProps {
  modelStatus: ModelStatus;
  recordingStatus: RecordingStatus;
  onPressIn: () => void;
  onPressOut: () => void;
  onPress: () => void;
}

export function VoiceButton({
  modelStatus,
  recordingStatus,
  onPressIn,
  onPressOut,
  onPress,
}: VoiceButtonProps) {
  const colors = useThemeColors("dark");
  const scale = useSharedValue(1);
  const opacity = useSharedValue(1);

  const isRecording = recordingStatus === "recording";
  const isDisabled = modelStatus !== "ready";

  // Pulsing animation during recording
  useEffect(() => {
    if (isRecording) {
      scale.value = withRepeat(
        withSequence(
          withTiming(1.15, { duration: 600 }),
          withTiming(1.0, { duration: 600 })
        ),
        -1,
        true
      );
      opacity.value = withRepeat(
        withSequence(
          withTiming(0.7, { duration: 600 }),
          withTiming(1.0, { duration: 600 })
        ),
        -1,
        true
      );
    } else {
      cancelAnimation(scale);
      cancelAnimation(opacity);
      scale.value = withTiming(1, { duration: 200 });
      opacity.value = withTiming(1, { duration: 200 });
    }
    return () => {
      cancelAnimation(scale);
      cancelAnimation(opacity);
    };
  }, [isRecording, scale, opacity]);

  const animatedStyle = useAnimatedStyle(() => ({
    transform: [{ scale: scale.value }],
    opacity: opacity.value,
  }));

  const iconColor = isDisabled
    ? colors.separator
    : isRecording
      ? colors.danger
      : colors.tint;

  const accessibilityLabel = isRecording
    ? "Recording, release to stop"
    : modelStatus !== "ready"
      ? "Voice model required"
      : "Hold to speak";

  const accessibilityHint = isDisabled
    ? "Tap to open voice model download"
    : "Press and hold to record voice, release to transcribe";

  return (
    <Animated.View style={animatedStyle}>
      <Pressable
        onPressIn={isDisabled ? undefined : onPressIn}
        onPressOut={isDisabled ? undefined : onPressOut}
        onPress={isDisabled ? onPress : undefined}
        style={[
          styles.button,
          { backgroundColor: colors.surface },
        ]}
        accessibilityRole="button"
        accessibilityLabel={accessibilityLabel}
        accessibilityHint={accessibilityHint}
        testID="voice-button"
      >
        <FontAwesome
          name="microphone"
          size={20}
          color={iconColor}
        />
      </Pressable>
    </Animated.View>
  );
}

const styles = StyleSheet.create({
  button: {
    width: 48,
    height: 48,
    borderRadius: 24,
    alignItems: "center",
    justifyContent: "center",
  },
});
