import type { ComponentProps } from "react";

import { ErrorView, type ErrorWithAction } from "@/components/ErrorView";
import { useConnection } from "@/lib/ConnectionContext";
import {
  resolveMappedErrorAction,
  toMappedError,
  type ErrorAction,
  type ErrorLike,
} from "@/lib/errorMessages";

const DEFAULT_ACTION_LABELS: Record<ErrorWithAction, string> = {
  retry: "Retry",
  "re-pair": "Re-scan QR Code",
  "go-back": "Re-scan QR Code",
};

const RETRY_ONLY_ACTION_LABELS: Record<ErrorWithAction, string> = {
  retry: "Retry",
  "re-pair": "Retry",
  "go-back": "Retry",
};

type BaseErrorViewProps = Omit<
  ComponentProps<typeof ErrorView>,
  "error" | "actionHandlers" | "actionLabels"
>;

type ConnectionErrorVariant = "default" | "retry-only";

export interface ConnectionErrorViewProps extends BaseErrorViewProps {
  error: ErrorLike;
  onRetry?: () => void;
  onRePair?: () => void;
  onGoBack?: () => void;
  onRecover?: () => void;
  actionLabels?: Partial<Record<ErrorWithAction, string>>;
  variant?: ConnectionErrorVariant;
  useConnectionRetry?: boolean;
}

function getDefaultAccessibility(action: ErrorAction): {
  label?: string;
  hint?: string;
} {
  if (action === "retry") {
    return {
      label: "Retry connection",
      hint: "Attempts to reconnect to nupid",
    };
  }

  if (action === "re-pair" || action === "go-back") {
    return {
      label: "Re-scan QR Code",
      hint: "Opens camera to scan a QR code for pairing with nupid",
    };
  }

  return {};
}

function getDefaultButtonTestID(action: ErrorAction): string | undefined {
  if (action === "retry") return "retry-connection-button";
  if (action === "re-pair" || action === "go-back") return "scan-qr-button";
  return undefined;
}

export function ConnectionErrorView({
  error,
  onRetry,
  onRePair,
  onGoBack,
  onRecover,
  actionLabels,
  variant = "default",
  useConnectionRetry = true,
  accessibilityLabel,
  accessibilityHint,
  buttonTestID,
  ...rest
}: ConnectionErrorViewProps) {
  const { retryConnection } = useConnection();
  const mapped = toMappedError(error);
  const action = resolveMappedErrorAction(mapped);

  const retryHandler = onRetry ?? (useConnectionRetry ? retryConnection : undefined);
  const recoverHandler = variant === "retry-only" ? retryHandler : onRecover;
  const resolvedOnRePair = onRePair ?? recoverHandler;
  const resolvedOnGoBack = onGoBack ?? recoverHandler;
  const defaultActionLabels =
    variant === "retry-only" ? RETRY_ONLY_ACTION_LABELS : DEFAULT_ACTION_LABELS;
  const defaultAccessibility = getDefaultAccessibility(action);

  return (
    <ErrorView
      {...rest}
      error={error}
      actionHandlers={{
        retry: retryHandler,
        "re-pair": resolvedOnRePair,
        "go-back": resolvedOnGoBack,
      }}
      actionLabels={{
        ...defaultActionLabels,
        ...actionLabels,
      }}
      accessibilityLabel={accessibilityLabel ?? defaultAccessibility.label}
      accessibilityHint={accessibilityHint ?? defaultAccessibility.hint}
      buttonTestID={buttonTestID ?? getDefaultButtonTestID(action)}
    />
  );
}
