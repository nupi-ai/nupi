import { sharedDesignTokens } from "@nupi/shared/design-tokens";

const tintColorLight = "#1977b5";
const tintColorDark = sharedDesignTokens.color.white;

export interface ColorPalette {
  text: string;
  background: string;
  tint: string;
  danger: string;
  success: string;
  warning: string;
  onWarning: string;
  onDanger: string;
  surface: string;
  separator: string;
  overlay: string;
  onOverlay: string;
  terminalBackground: string;
}

const colors: { light: ColorPalette; dark: ColorPalette } = {
  light: {
    text: sharedDesignTokens.color.black,
    background: sharedDesignTokens.color.white,
    tint: tintColorLight,
    danger: "#dc2626",
    success: "#22c55e",
    warning: "#eab308",
    onWarning: sharedDesignTokens.color.black,
    onDanger: sharedDesignTokens.color.white,
    surface: "#f0f0f0",
    separator: "#ddd",
    overlay: "rgba(0,0,0,0.5)",
    onOverlay: sharedDesignTokens.color.white,
    terminalBackground: sharedDesignTokens.color.terminalBackground,
  },
  dark: {
    text: sharedDesignTokens.color.white,
    background: sharedDesignTokens.color.black,
    tint: tintColorDark,
    danger: "#ff4444",
    success: "#22c55e",
    warning: "#eab308",
    onWarning: sharedDesignTokens.color.black,
    onDanger: sharedDesignTokens.color.white,
    surface: "#1c1c1e",
    separator: sharedDesignTokens.color.neutral333,
    overlay: sharedDesignTokens.overlay.black70,
    onOverlay: sharedDesignTokens.color.white,
    terminalBackground: sharedDesignTokens.color.terminalBackground,
  },
};

export default colors;
