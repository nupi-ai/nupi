import type { CSSProperties } from "react";

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
  backgroundColor: "#1a1a1a",
  borderBottom: "1px solid #333",
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
  backgroundColor: "#333",
  color: "#fff",
};

const navButtonInactive: CSSProperties = {
  ...navButtonBase,
  backgroundColor: "transparent",
  color: "#999",
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
  color: "#666",
  marginTop: "1rem",
};

export const statusBar: CSSProperties = {
  height: "32px",
  backgroundColor: "#1a1a1a",
  borderTop: "1px solid #333",
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
  color: "#999",
};

export const statusIcon: CSSProperties = {
  fontSize: "16px",
};

const statusTextError: CSSProperties = { color: "#f87171" };
const statusTextRunning: CSSProperties = { color: "#4ade80" };
const statusTextStarting: CSSProperties = { color: "#fbbf24" };

export function statusText(hasError: boolean, isRunning: boolean): CSSProperties {
  if (hasError) return statusTextError;
  return isRunning ? statusTextRunning : statusTextStarting;
}

export const versionToast: CSSProperties = {
  position: "fixed",
  top: "16px",
  right: "16px",
  backgroundColor: "#78350f",
  color: "#fef3c7",
  padding: "12px 16px",
  borderRadius: "8px",
  fontSize: "13px",
  zIndex: 2000,
  maxWidth: "400px",
  boxShadow: "0 4px 12px rgba(0,0,0,0.3)",
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
  color: "#fef3c7",
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
  backgroundColor: "rgba(0, 0, 0, 0.9)",
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
  backgroundColor: "#0d1117",
  borderRadius: "8px",
  display: "flex",
  flexDirection: "column",
};

export const recordingHeader: CSSProperties = {
  padding: "12px 16px",
  borderBottom: "1px solid #333",
  display: "flex",
  alignItems: "center",
  justifyContent: "space-between",
  backgroundColor: "#1a1a1a",
};

export const recordingTitle: CSSProperties = {
  margin: 0,
  fontSize: "16px",
};

export const recordingCloseButton: CSSProperties = {
  padding: "4px 12px",
  backgroundColor: "#333",
  border: "none",
  borderRadius: "4px",
  color: "#fff",
  cursor: "pointer",
  fontSize: "14px",
};

export const recordingContent: CSSProperties = {
  flex: 1,
  padding: "16px",
  backgroundColor: "#0d1117",
  position: "relative",
  minHeight: 0,
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
};

export const recordingLoadingText: CSSProperties = {
  color: "#999",
};
