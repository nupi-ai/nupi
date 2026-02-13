import type { CSSProperties } from "react";

export const root: CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: "16px",
  padding: "16px",
  color: "#fff",
  backgroundColor: "#121212",
  overflowY: "auto",
  height: "100%",
};

export const section: CSSProperties = {
  backgroundColor: "#1c1c1c",
  borderRadius: "8px",
  padding: "16px",
  border: "1px solid #2a2a2a",
  boxShadow: "0 4px 12px rgba(0,0,0,0.2)",
};

export const sectionHeading: CSSProperties = { marginTop: 0 };

export const descriptionText: CSSProperties = {
  color: "#9ca3af",
  lineHeight: 1.5,
};

export const labelText: CSSProperties = {
  fontSize: "0.85rem",
  color: "#9ca3af",
};

export const statusText: CSSProperties = {
  marginTop: "16px",
  color: "#9ca3af",
  fontSize: "0.9rem",
};

export const infoText: CSSProperties = {
  color: "#6b7280",
  fontSize: "0.85rem",
};

export const warningText: CSSProperties = {
  color: "#f87171",
  fontSize: "0.85rem",
};

export const formGrid: CSSProperties = {
  display: "grid",
  gap: "12px",
  gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))",
  alignItems: "flex-end",
};

export const fieldLabel: CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: "6px",
};

export const textInput: CSSProperties = {
  backgroundColor: "#111",
  color: "#fff",
  border: "1px solid #333",
  borderRadius: "6px",
  padding: "8px 10px",
};

export const buttonRow: CSSProperties = {
  display: "flex",
  flexWrap: "wrap",
  gap: "12px",
  marginTop: "16px",
};

export const actionButtonRow: CSSProperties = {
  display: "flex",
  flexWrap: "wrap",
  gap: "12px",
  marginTop: "20px",
};

const buttonBase: CSSProperties = {
  borderRadius: "6px",
  cursor: "pointer",
};

export const primaryButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 18px",
  backgroundColor: "#2563eb",
  color: "#fff",
  border: "none",
};

export const secondaryButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 18px",
  backgroundColor: "#1f2937",
  color: "#e5e7eb",
  border: "1px solid #374151",
};

export const dangerOutlineButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 18px",
  backgroundColor: "transparent",
  color: "#f87171",
  border: "1px solid #f87171",
};

export const successButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: "#10b981",
  color: "#fff",
  border: "none",
};

export const warningButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: "#f97316",
  color: "#fff",
  border: "none",
};

export const indigoButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: "#4338ca",
  color: "#fff",
  border: "none",
};

export const dangerButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: "#ef4444",
  color: "#fff",
  border: "none",
};

export const checkboxLabel: CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: "8px",
  color: "#cbd5f5",
};

const preBlock: CSSProperties = {
  backgroundColor: "#0f172a",
  padding: "12px",
  borderRadius: "6px",
  fontSize: "0.85rem",
  overflow: "auto",
};

export const capabilitiesPre: CSSProperties = {
  ...preBlock,
  maxHeight: "220px",
};

export const resultPre: CSSProperties = {
  ...preBlock,
  maxHeight: "240px",
};

const cursorWait: CSSProperties = { cursor: "wait" };
const cursorPointer: CSSProperties = { cursor: "pointer" };

export function busyCursor(isBusy: boolean): CSSProperties {
  return isBusy ? cursorWait : cursorPointer;
}
