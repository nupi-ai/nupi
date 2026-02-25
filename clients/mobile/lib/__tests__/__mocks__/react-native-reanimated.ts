/**
 * Minimal react-native-reanimated stub for vitest tests.
 */
import React from "react";

const AnimatedView = ({ children, style }: any) =>
  React.createElement("div", { style }, children);

export default {
  View: AnimatedView,
  createAnimatedComponent: (c: any) => c,
};

export function useSharedValue(init: number) { return { value: init }; }
export function useAnimatedStyle(factory: () => any) { return factory(); }
export function withRepeat() { return 0; }
export function withSequence() { return 0; }
export function withTiming() { return 0; }
export function cancelAnimation() {}

export const Easing = {
  inOut: () => ({}),
  ease: {},
};
