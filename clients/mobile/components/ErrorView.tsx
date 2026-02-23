import {
  StyleSheet,
  type StyleProp,
  type TextStyle,
  type ViewStyle,
  View as RNView,
} from "react-native";

import { Button } from "@/components/Button";
import { Text } from "@/components/Themed";
import Colors from "@/constants/Colors";
import { useColorScheme } from "@/components/useColorScheme";
import type { ErrorAction, MappedError } from "@/lib/errorMessages";

type ErrorWithAction = Exclude<ErrorAction, "none">;

const DEFAULT_ACTION_LABELS: Record<ErrorWithAction, string> = {
  retry: "Retry",
  "re-pair": "Re-scan QR Code",
  "go-back": "Go Back",
};

interface ErrorViewProps {
  error: string | MappedError;
  onRetry?: () => void;
  onRePair?: () => void;
  onGoBack?: () => void;
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

function toMappedError(error: string | MappedError): MappedError {
  if (typeof error === "string") {
    return {
      message: error,
      action: "none",
      canRetry: false,
    };
  }
  return error;
}

function resolveActionHandler(
  action: ErrorAction,
  handlers: {
    onRetry?: () => void;
    onRePair?: () => void;
    onGoBack?: () => void;
  }
): (() => void) | undefined {
  if (action === "retry") return handlers.onRetry;
  if (action === "re-pair") return handlers.onRePair;
  if (action === "go-back") return handlers.onGoBack;
  return undefined;
}

export function ErrorView({
  error,
  onRetry,
  onRePair,
  onGoBack,
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

  const action: ErrorAction =
    mapped.action === "retry" && !mapped.canRetry ? "none" : mapped.action;
  const onPress = resolveActionHandler(action, { onRetry, onRePair, onGoBack });
  const actionLabel =
    action === "none"
      ? null
      : actionLabels?.[action] ?? DEFAULT_ACTION_LABELS[action];
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
