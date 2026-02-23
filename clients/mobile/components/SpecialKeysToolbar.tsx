import { useCallback } from "react";
import {
  Platform,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  View as RNView,
} from "react-native";

import { useThemeColors } from "@/components/useColorScheme";

/** Escape sequences for special terminal keys. */
const KEYS = {
  ESC: "\x1b",
  TAB: "\t",
  UP: "\x1b[A",
  DOWN: "\x1b[B",
  LEFT: "\x1b[D",
  RIGHT: "\x1b[C",
} as const;

interface SpecialKeysToolbarProps {
  onSendInput: (data: string) => void;
  ctrlActive: boolean;
  onCtrlToggle: () => void;
}

export function SpecialKeysToolbar({
  onSendInput,
  ctrlActive,
  onCtrlToggle,
}: SpecialKeysToolbarProps) {
  const colors = useThemeColors("dark");

  const sendKey = useCallback(
    (seq: string) => {
      onSendInput(seq);
    },
    [onSendInput]
  );

  const keyColor = colors.text;
  const bgColor = colors.surface;
  const ctrlBg = ctrlActive
    ? colors.tint
    : colors.surface;
  const ctrlTextColor = ctrlActive
    ? colors.background
    : colors.text;

  return (
    <RNView
      style={[styles.container, { backgroundColor: colors.terminalBackground }]}
      accessibilityRole="toolbar"
      accessibilityLabel="Special keys toolbar"
    >
      <ScrollView
        horizontal
        showsHorizontalScrollIndicator={false}
        keyboardShouldPersistTaps="always"
        contentContainerStyle={styles.scrollContent}
      >
        <Pressable
          onPress={() => sendKey(KEYS.ESC)}
          style={[styles.key, { backgroundColor: bgColor }]}
          accessibilityRole="button"
          accessibilityLabel="Escape key"
          testID="special-key-esc"
        >
          <Text style={[styles.keyText, { color: keyColor }]}>Esc</Text>
        </Pressable>

        <Pressable
          onPress={() => sendKey(KEYS.TAB)}
          style={[styles.key, { backgroundColor: bgColor }]}
          accessibilityRole="button"
          accessibilityLabel="Tab key"
          testID="special-key-tab"
        >
          <Text style={[styles.keyText, { color: keyColor }]}>Tab</Text>
        </Pressable>

        <Pressable
          onPress={onCtrlToggle}
          style={[styles.key, { backgroundColor: ctrlBg }]}
          accessibilityRole="button"
          accessibilityLabel={ctrlActive ? "Ctrl modifier active" : "Ctrl modifier"}
          accessibilityHint="Double tap to toggle. When active, the next letter key sends a Ctrl combination"
          testID="special-key-ctrl"
          accessibilityState={{ selected: ctrlActive }}
        >
          <Text style={[styles.keyText, { color: ctrlTextColor }]}>Ctrl</Text>
        </Pressable>

        <Pressable
          onPress={() => sendKey(KEYS.UP)}
          style={[styles.key, { backgroundColor: bgColor }]}
          accessibilityRole="button"
          accessibilityLabel="Up arrow key"
          testID="special-key-up"
        >
          <Text style={[styles.keyText, { color: keyColor }]}>↑</Text>
        </Pressable>

        <Pressable
          onPress={() => sendKey(KEYS.DOWN)}
          style={[styles.key, { backgroundColor: bgColor }]}
          accessibilityRole="button"
          accessibilityLabel="Down arrow key"
          testID="special-key-down"
        >
          <Text style={[styles.keyText, { color: keyColor }]}>↓</Text>
        </Pressable>

        <Pressable
          onPress={() => sendKey(KEYS.LEFT)}
          style={[styles.key, { backgroundColor: bgColor }]}
          accessibilityRole="button"
          accessibilityLabel="Left arrow key"
          testID="special-key-left"
        >
          <Text style={[styles.keyText, { color: keyColor }]}>←</Text>
        </Pressable>

        <Pressable
          onPress={() => sendKey(KEYS.RIGHT)}
          style={[styles.key, { backgroundColor: bgColor }]}
          accessibilityRole="button"
          accessibilityLabel="Right arrow key"
          testID="special-key-right"
        >
          <Text style={[styles.keyText, { color: keyColor }]}>→</Text>
        </Pressable>
      </ScrollView>
    </RNView>
  );
}

const styles = StyleSheet.create({
  container: {
    paddingVertical: 6,
    paddingHorizontal: 4,
  },
  scrollContent: {
    flexDirection: "row",
    alignItems: "center",
    gap: 6,
    paddingHorizontal: 4,
  },
  key: {
    paddingHorizontal: 14,
    paddingVertical: 13,
    borderRadius: 6,
    minWidth: 44,
    alignItems: "center",
    justifyContent: "center",
  },
  keyText: {
    fontSize: 14,
    fontWeight: "600",
    fontFamily: Platform.select({ ios: "Menlo", default: "monospace" }),
  },
});
