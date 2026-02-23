import type { CSSProperties } from "react";
import { theme } from "./designTokens";

const borderDefault = `1px solid ${theme.border.default}`;

export const root: CSSProperties = {
  display: "flex",
  flexDirection: "column",
  height: "100vh",
  minHeight: 0,
  overflow: "hidden",
};

export const main: CSSProperties = {
  flex: 1,
  display: "flex",
  flexDirection: "column",
  padding: 0,
  minHeight: 0,
  overflow: "hidden",
};

export const navTabs: CSSProperties = {
  display: "flex",
  backgroundColor: theme.bg.nav,
  borderBottom: borderDefault,
  padding: "8px 16px",
  gap: "8px",
};

const navButtonBase: CSSProperties = {
  padding: "6px 16px",
  border: "none",
  borderRadius: "4px",
  cursor: "pointer",
  fontSize: "14px",
  transition: "all 0.2s",
};

const navButtonActive: CSSProperties = {
  ...navButtonBase,
  backgroundColor: theme.bg.neutral,
  color: theme.text.primary,
};

const navButtonInactive: CSSProperties = {
  ...navButtonBase,
  backgroundColor: theme.bg.transparent,
  color: theme.text.muted,
};

export function navButton(isActive: boolean): CSSProperties {
  return isActive ? navButtonActive : navButtonInactive;
}

export const waitingContainer: CSSProperties = {
  paddingBottom: "40px",
};

export const waitingTitle: CSSProperties = {
  fontSize: "1.2rem",
  marginTop: "2rem",
};

export const waitingDescription: CSSProperties = {
  fontSize: "0.9rem",
  color: theme.text.subdued,
  marginTop: "1rem",
};

export const statusBar: CSSProperties = {
  height: "32px",
  backgroundColor: theme.bg.nav,
  borderTop: borderDefault,
  display: "flex",
  alignItems: "center",
  paddingLeft: "12px",
  fontSize: "13px",
  fontFamily: "system-ui, -apple-system, sans-serif",
  flexShrink: 0,
};

export const statusLabelGroup: CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: "6px",
  color: theme.text.muted,
};

export const statusIcon: CSSProperties = {
  fontSize: "16px",
};

const statusTextError: CSSProperties = { color: theme.text.danger };
const statusTextRunning: CSSProperties = { color: theme.text.success };
const statusTextStarting: CSSProperties = { color: theme.text.warning };

export function statusText(hasError: boolean, isRunning: boolean): CSSProperties {
  if (hasError) return statusTextError;
  return isRunning ? statusTextRunning : statusTextStarting;
}

export const versionToast: CSSProperties = {
  position: "fixed",
  top: "16px",
  right: "16px",
  backgroundColor: theme.bg.toast,
  color: theme.text.toast,
  padding: "12px 16px",
  borderRadius: "8px",
  fontSize: "13px",
  zIndex: 2000,
  maxWidth: "400px",
  boxShadow: `0 4px 12px ${theme.shadow.toast}`,
  display: "flex",
  alignItems: "center",
  gap: "8px",
};

export const versionToastIcon: CSSProperties = {
  flexShrink: 0,
};

export const versionToastMessage: CSSProperties = {
  flex: 1,
};

export const versionToastClose: CSSProperties = {
  background: "none",
  border: "none",
  color: theme.text.toast,
  cursor: "pointer",
  fontSize: "16px",
  padding: "0 4px",
  flexShrink: 0,
};

export const recordingOverlay: CSSProperties = {
  position: "fixed",
  top: 0,
  left: 0,
  right: 0,
  bottom: 0,
  backgroundColor: theme.bg.overlay,
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  zIndex: 1000,
  padding: "32px",
};

export const recordingModal: CSSProperties = {
  width: "90%",
  height: "90%",
  maxWidth: "1200px",
  maxHeight: "800px",
  backgroundColor: theme.bg.modal,
  borderRadius: "8px",
  display: "flex",
  flexDirection: "column",
};

export const recordingHeader: CSSProperties = {
  padding: "12px 16px",
  borderBottom: borderDefault,
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  backgroundColor: theme.bg.nav,
};

export const recordingTitle: CSSProperties = {
  margin: 0,
  fontSize: "16px",
};

export const recordingCloseButton: CSSProperties = {
  padding: "4px 12px",
  backgroundColor: theme.bg.neutral,
  border: "none",
  borderRadius: "4px",
  color: theme.text.primary,
  cursor: "pointer",
  fontSize: "14px",
};

export const recordingContent: CSSProperties = {
  flex: 1,
  padding: "16px",
  backgroundColor: theme.bg.modal,
  position: "relative",
  minHeight: 0,
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
};

export const recordingLoadingText: CSSProperties = {
  color: theme.text.muted,
};
