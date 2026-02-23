import {
  StyleSheet,
  type StyleProp,
  type TextStyle,
  type ViewStyle,
  View as RNView,
} from "react-native";

import { Button } from "@/components/Button";
import { Text } from "@/components/Themed";
import Colors from "@/constants/designTokens";
import { useColorScheme } from "@/components/useColorScheme";
import {
  resolveMappedErrorAction,
  toMappedError,
  type ErrorAction,
  type ErrorLike,
} from "@/lib/errorMessages";

export type ErrorWithAction = Exclude<ErrorAction, "none">;

const DEFAULT_ACTION_LABELS: Record<ErrorWithAction, string> = {
  retry: "Retry",
  "re-pair": "Re-scan QR Code",
  "go-back": "Go Back",
};

interface ErrorViewProps {
  error: ErrorLike;
  actionHandlers?: Partial<Record<ErrorWithAction, () => void>>;
  fallbackText?: string;
  actionLabels?: Partial<Record<ErrorWithAction, string>>;
  accessibilityLabel?: string;
  accessibilityHint?: string;
  buttonTestID?: string;
  containerStyle?: StyleProp<ViewStyle>;
  messageStyle?: StyleProp<TextStyle>;
  fallbackStyle?: StyleProp<TextStyle>;
  buttonStyle?: StyleProp<ViewStyle>;
  buttonTextStyle?: StyleProp<TextStyle>;
  messageColor?: string;
  buttonColor?: string;
  buttonTextColor?: string;
}

function resolveActionHandler(
  action: ErrorAction,
  handlers?: Partial<Record<ErrorWithAction, () => void>>
): (() => void) | undefined {
  if (action === "none") return undefined;
  return handlers?.[action];
}

function resolveActionLabel(
  action: ErrorAction,
  actionLabels?: Partial<Record<ErrorWithAction, string>>
): string | null {
  if (action === "none") return null;
  return actionLabels?.[action] ?? DEFAULT_ACTION_LABELS[action];
}

export function ErrorView({
  error,
  actionHandlers,
  fallbackText,
  actionLabels,
  accessibilityLabel,
  accessibilityHint,
  buttonTestID,
  containerStyle,
  messageStyle,
  fallbackStyle,
  buttonStyle,
  buttonTextStyle,
  messageColor,
  buttonColor,
  buttonTextColor,
}: ErrorViewProps) {
  const colorScheme = useColorScheme() ?? "light";
  const colors = Colors[colorScheme];
  const mapped = toMappedError(error);

  const action = resolveMappedErrorAction(mapped);
  const onPress = resolveActionHandler(action, actionHandlers);
  const actionLabel = resolveActionLabel(action, actionLabels);
  const showActionButton = Boolean(actionLabel && onPress);

  return (
    <RNView style={[styles.container, containerStyle]}>
      <Text style={[styles.message, { color: messageColor ?? colors.danger }, messageStyle]}>
        {mapped.message}
      </Text>

      {showActionButton && onPress ? (
        <Button
          style={[styles.button, buttonStyle]}
          variant="primary"
          color={buttonColor ?? colors.tint}
          onPress={onPress}
          accessibilityLabel={accessibilityLabel ?? actionLabel ?? undefined}
          accessibilityHint={accessibilityHint}
          testID={buttonTestID}
        >
          <Text
            style={[
              styles.buttonText,
              { color: buttonTextColor ?? colors.background },
              buttonTextStyle,
            ]}
          >
            {actionLabel}
          </Text>
        </Button>
      ) : (
        fallbackText && <Text style={[styles.fallbackText, fallbackStyle]}>{fallbackText}</Text>
      )}
    </RNView>
  );
}

const styles = StyleSheet.create({
  container: {
    alignItems: "center",
  },
  message: {
    fontSize: 14,
    textAlign: "center",
  },
  fallbackText: {
    fontSize: 16,
    textAlign: "center",
    opacity: 0.7,
  },
  button: {
    paddingHorizontal: 24,
    paddingVertical: 12,
    borderRadius: 12,
  },
  buttonText: {
    fontSize: 16,
    fontWeight: "600",
  },
});
