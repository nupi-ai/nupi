/**
 * Minimal react-native stub for vitest tests that import components
 * using react-native APIs. This prevents Flow syntax parse errors
 * from the real react-native package.
 *
 * Maps React Native accessibility/test props to standard DOM attributes:
 *   testID → data-testid
 *   accessibilityRole → role
 *   accessibilityLabel → aria-label
 *   accessibilityLiveRegion → aria-live
 */
import React from "react";

export const Platform = { OS: "ios" as const, select: (spec: any) => spec.ios };

export const StyleSheet = {
  create: <T extends Record<string, unknown>>(styles: T): T => styles,
};

/** Map RN props to DOM-compatible equivalents. */
export function mapRNProps(props: Record<string, any>): Record<string, any> {
  const {
    testID,
    accessibilityRole,
    accessibilityLabel,
    accessibilityLiveRegion,
    accessibilityState,
    numberOfLines,
    ellipsizeMode,
    ...rest
  } = props;
  const mapped: Record<string, any> = { ...rest };
  if (testID) mapped["data-testid"] = testID;
  if (accessibilityRole) mapped.role = accessibilityRole;
  if (accessibilityLabel) mapped["aria-label"] = accessibilityLabel;
  if (accessibilityLiveRegion) mapped["aria-live"] = accessibilityLiveRegion;
  if (accessibilityState) {
    if (accessibilityState.disabled) mapped["aria-disabled"] = "true";
    if (accessibilityState.selected) mapped["aria-selected"] = "true";
    if (accessibilityState.busy) mapped["aria-busy"] = "true";
    if (accessibilityState.expanded != null) mapped["aria-expanded"] = String(accessibilityState.expanded);
  }
  return mapped;
}

export const View = React.forwardRef(({ children, ...props }: any, ref: any) =>
  React.createElement("div", { ...mapRNProps(props), ref }, children),
);

export const FlatList = React.forwardRef(
  ({
    data,
    renderItem,
    keyExtractor,
    ListFooterComponent,
    contentContainerStyle,
    onScroll,
    scrollEventThrottle,
    onContentSizeChange,
    removeClippedSubviews,
    maxToRenderPerBatch,
    ...props
  }: any, ref: any) =>
    React.createElement(
      "div",
      { ...mapRNProps(props), ref },
      ...(data ?? []).map((item: any, index: number) =>
        React.createElement(
          "div",
          { key: keyExtractor?.(item, index) ?? index },
          renderItem({ item, index }),
        ),
      ),
      ListFooterComponent
        ? typeof ListFooterComponent === "function"
          ? React.createElement(ListFooterComponent)
          : ListFooterComponent
        : null,
    ),
);

export const Text = ({ children, ...props }: any) =>
  React.createElement("span", mapRNProps(props), children);

export const Pressable = ({ children, ...props }: any) =>
  React.createElement("button", mapRNProps(props), children);

export const ActivityIndicator = () => null;

export const Keyboard = {
  addListener: () => ({ remove: () => {} }),
  dismiss: () => {},
};

export const KeyboardAvoidingView = ({ children }: any) => children;

export const PermissionsAndroid = {
  request: async () => "granted",
  PERMISSIONS: { RECORD_AUDIO: "" },
  RESULTS: { GRANTED: "granted", NEVER_ASK_AGAIN: "never_ask_again" },
};

export function useWindowDimensions() {
  return { width: 400, height: 800 };
}
