import React, { memo, useEffect, useMemo, useRef } from "react";
import {
  FlatList,
  type NativeScrollEvent,
  type NativeSyntheticEvent,
  StyleSheet,
  useWindowDimensions,
  View as RNView,
  type ViewStyle,
} from "react-native";
import Animated, {
  cancelAnimation,
  useAnimatedStyle,
  useSharedValue,
  withRepeat,
  withSequence,
  withTiming,
} from "react-native-reanimated";

import { Text } from "@/components/Themed";
import { useThemeColors } from "@/components/useColorScheme";
import type { ConversationTurn } from "@/lib/useConversation";

interface ConversationPanelProps {
  turns: ConversationTurn[];
  isThinking: boolean;
  /** Poll/fetch error message to display inline. */
  pollError?: string | null;
  /** Voice command send error to display inline. */
  sendError?: boolean;
  style?: ViewStyle;
}

const MAX_HEIGHT_RATIO = 0.4;

function formatTime(date: Date): string {
  const h = date.getHours().toString().padStart(2, "0");
  const m = date.getMinutes().toString().padStart(2, "0");
  const now = new Date();
  const isToday =
    date.getDate() === now.getDate() &&
    date.getMonth() === now.getMonth() &&
    date.getFullYear() === now.getFullYear();
  if (isToday) return `${h}:${m}`;
  const d = date.getDate().toString().padStart(2, "0");
  const mo = (date.getMonth() + 1).toString().padStart(2, "0");
  return `${d}/${mo} ${h}:${m}`;
}

/** Parse action metadata on AI turns for command/speak annotations. */
function getActionAnnotations(metadata: Record<string, string>): string[] {
  const annotations: string[] = [];
  for (let i = 0; ; i++) {
    const type = metadata[`action_${i}.type`];
    if (!type) break;
    if (type === "command") {
      const target = metadata[`action_${i}.target`];
      if (target) {
        annotations.push(`Command sent to ${target}`);
      }
    } else if (type === "speak") {
      const speakText = metadata[`action_${i}.text`];
      if (speakText) {
        annotations.push(`Speaking: "${speakText}"`);
      }
    }
  }
  return annotations;
}

// L1 fix: ThinkingIndicator only receives `visible` — animation starts/stops conditionally.
function ThinkingIndicator({ visible }: { visible: boolean }) {
  const opacity = useSharedValue(0.3);

  useEffect(() => {
    if (visible) {
      opacity.value = withRepeat(
        withSequence(
          withTiming(1, { duration: 600 }),
          withTiming(0.3, { duration: 600 }),
        ),
        -1,
        true,
      );
    } else {
      cancelAnimation(opacity);
      opacity.value = 0.3;
    }
    return () => {
      cancelAnimation(opacity);
    };
  }, [visible, opacity]);

  const animatedStyle = useAnimatedStyle(() => ({
    opacity: opacity.value,
  }));

  const colors = useThemeColors("dark");

  if (!visible) return null;

  return (
    <RNView
      style={[styles.aiBubble, { backgroundColor: colors.surface }]}
      accessibilityRole="text"
      accessibilityLabel="AI is thinking"
      accessibilityLiveRegion="polite"
    >
      <Animated.View style={animatedStyle}>
        <Text style={[styles.thinkingDots, { color: colors.text }]}>
          {"\u2022\u2022\u2022"}
        </Text>
      </Animated.View>
    </RNView>
  );
}

const MessageBubble = memo(function MessageBubble({ turn, index }: { turn: ConversationTurn; index: number }) {
  const colors = useThemeColors("dark");

  // System/tool messages: centered, italic
  if (turn.origin === "system" || turn.origin === "tool") {
    return (
      <RNView
        style={styles.systemRow}
        accessibilityRole="text"
        accessibilityLabel={`${turn.origin} says: ${turn.text}`}
        testID={`conversation-message-${index}`}
      >
        <Text style={[styles.systemText, { color: colors.text }]}>
          {turn.text}
        </Text>
        <Text style={[styles.timestamp, { color: colors.text }]}>
          {formatTime(turn.at)}
        </Text>
      </RNView>
    );
  }

  // User messages: right-aligned
  if (turn.origin === "user") {
    return (
      <RNView
        style={[
          styles.userBubble,
          {
            backgroundColor: colors.tint,
            opacity: turn.isOptimistic ? 0.7 : 1,
          },
        ]}
        accessibilityRole="text"
        accessibilityLabel={`user says: ${turn.text}`}
        testID={`conversation-message-${index}`}
      >
        <Text style={[styles.userText, { color: colors.background }]}>
          {turn.text}
        </Text>
        <Text style={[styles.timestamp, { color: colors.background }]}>
          {formatTime(turn.at)}
        </Text>
      </RNView>
    );
  }

  // AI messages: left-aligned
  const annotations = getActionAnnotations(turn.metadata);

  return (
    <RNView
      style={styles.aiRow}
      accessibilityRole="text"
      accessibilityLabel={`ai says: ${turn.text}`}
      accessibilityLiveRegion="polite"
      testID={`conversation-message-${index}`}
    >
      <RNView style={[styles.aiBubble, { backgroundColor: colors.surface }]}>
        <Text style={[styles.aiText, { color: colors.text }]}>
          {turn.text}
        </Text>
      </RNView>
      {annotations.map((annotation, i) => (
        <Text
          key={i}
          style={[styles.actionAnnotation, { color: colors.tint }]}
        >
          [{annotation}]
        </Text>
      ))}
      <Text style={[styles.timestamp, { color: colors.text }]}>
        {formatTime(turn.at)}
      </Text>
    </RNView>
  );
});

export function ConversationPanel({ turns, isThinking, pollError, sendError, style }: ConversationPanelProps) {
  const colors = useThemeColors("dark");
  const { height: windowHeight } = useWindowDimensions();
  const maxHeight = windowHeight * MAX_HEIGHT_RATIO;
  const listRef = useRef<FlatList<ConversationTurn>>(null);
  const prevCountRef = useRef(turns.length);
  const shouldAutoScrollRef = useRef(true);
  // Stable key counter — increments monotonically for new turns.
  const nextKeyIdRef = useRef(0);
  // Map from deterministic turn identity to stable key string.
  const keyMapRef = useRef(new Map<string, string>());

  // M2 fix (Review 9): memoize key identity computation so it only re-runs
  // when `turns` changes, not on unrelated prop changes (isThinking, errors).
  const turnKeys = useMemo(() => {
    const keys = turns.map((turn) => {
      const identity = `${turn.origin}-${turn.at.getTime()}-${turn.isOptimistic ? "opt" : "real"}-${turn.text}`;
      const existing = keyMapRef.current.get(identity);
      if (existing) return existing;
      const key = `turn-${nextKeyIdRef.current++}`;
      keyMapRef.current.set(identity, key);
      return key;
    });
    // Prune map entries no longer in the current turns list.
    if (keyMapRef.current.size > turns.length * 2) {
      const activeIdentities = new Set(turns.map((turn) =>
        `${turn.origin}-${turn.at.getTime()}-${turn.isOptimistic ? "opt" : "real"}-${turn.text}`
      ));
      for (const id of keyMapRef.current.keys()) {
        if (!activeIdentities.has(id)) keyMapRef.current.delete(id);
      }
    }
    return keys;
  }, [turns]);

  // H1 fix: track scroll position to avoid auto-scroll when user reads older messages.
  const handleScroll = useRef((event: NativeSyntheticEvent<NativeScrollEvent>) => {
    const { contentOffset, contentSize, layoutMeasurement } = event.nativeEvent;
    const isAtBottom = contentOffset.y + layoutMeasurement.height >= contentSize.height - 50;
    shouldAutoScrollRef.current = isAtBottom;
  }).current;

  // Only force auto-scroll on new content when already at bottom.
  useEffect(() => {
    prevCountRef.current = turns.length;
  }, [turns.length]);

  return (
    <RNView
      style={[styles.container, { maxHeight }, style]}
      accessibilityRole="list"
      testID="conversation-panel"
    >
      <FlatList
        ref={listRef}
        data={turns}
        keyExtractor={(_item, index) => turnKeys[index] ?? `fallback-${index}`}
        renderItem={({ item, index }) => (
          <MessageBubble turn={item} index={index} />
        )}
        ListFooterComponent={
          <>
            <ThinkingIndicator visible={isThinking} />
            {sendError ? (
              <RNView style={styles.errorRow}>
                <Text
                  style={[styles.errorText, { color: colors.danger }]}
                  accessibilityRole="alert"
                  testID="conversation-send-error"
                >
                  Failed to send — tap mic to retry
                </Text>
              </RNView>
            ) : null}
            {pollError ? (
              <RNView style={styles.errorRow}>
                <Text
                  style={[styles.errorText, { color: colors.danger }]}
                  accessibilityRole="alert"
                  testID="conversation-poll-error"
                >
                  {pollError}
                </Text>
              </RNView>
            ) : null}
          </>
        }
        contentContainerStyle={styles.listContent}
        onScroll={handleScroll}
        scrollEventThrottle={16}
        onContentSizeChange={() => {
          if (shouldAutoScrollRef.current) {
            listRef.current?.scrollToEnd({ animated: true });
          }
        }}
        removeClippedSubviews
        maxToRenderPerBatch={10}
      />
    </RNView>
  );
}

const styles = StyleSheet.create({
  container: {
    paddingHorizontal: 8,
  },
  listContent: {
    paddingVertical: 4,
  },
  userBubble: {
    borderRadius: 12,
    padding: 10,
    maxWidth: "80%",
    alignSelf: "flex-end",
    marginVertical: 2,
  },
  userText: {
    fontSize: 14,
  },
  aiBubble: {
    borderRadius: 12,
    padding: 10,
    maxWidth: "80%",
    alignSelf: "flex-start",
    marginVertical: 2,
  },
  aiRow: {
    alignSelf: "flex-start",
    maxWidth: "80%",
    marginVertical: 2,
  },
  aiText: {
    fontSize: 14,
  },
  actionAnnotation: {
    fontSize: 12,
    fontStyle: "italic",
    marginTop: 2,
    marginLeft: 10,
  },
  systemRow: {
    alignSelf: "center",
    marginVertical: 2,
  },
  systemText: {
    fontSize: 13,
    fontStyle: "italic",
    opacity: 0.6,
    textAlign: "center",
  },
  timestamp: {
    fontSize: 11,
    opacity: 0.4,
    marginTop: 2,
  },
  thinkingDots: {
    fontSize: 20,
    letterSpacing: 4,
  },
  errorRow: {
    alignSelf: "center",
    marginVertical: 4,
  },
  errorText: {
    fontSize: 12,
    fontStyle: "italic",
    textAlign: "center",
  },
});
