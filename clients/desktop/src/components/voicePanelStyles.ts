import type { CSSProperties } from "react";
import { theme } from "../designTokens";

const borderDefault = `1px solid ${theme.border.default}`;
const borderDanger = `1px solid ${theme.border.danger}`;

export const root: CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: "16px",
  padding: "16px",
  color: theme.text.primary,
  backgroundColor: theme.bg.app,
  overflowY: "auto",
  height: "100%",
};

export const section: CSSProperties = {
  backgroundColor: theme.bg.panel,
  borderRadius: "8px",
  padding: "16px",
  border: `1px solid ${theme.border.subtle}`,
  boxShadow: `0 4px 12px ${theme.shadow.panel}`,
};

export const sectionHeading: CSSProperties = { marginTop: 0 };

export const descriptionText: CSSProperties = {
  color: theme.text.secondary,
  lineHeight: 1.5,
};

export const labelText: CSSProperties = {
  fontSize: "0.85rem",
  color: theme.text.secondary,
};

export const statusText: CSSProperties = {
  marginTop: "16px",
  color: theme.text.secondary,
  fontSize: "0.9rem",
};

export const infoText: CSSProperties = {
  color: theme.text.tertiary,
  fontSize: "0.85rem",
};

export const warningText: CSSProperties = {
  color: theme.text.danger,
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
  backgroundColor: theme.bg.input,
  color: theme.text.primary,
  border: borderDefault,
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
  backgroundColor: theme.bg.primaryButton,
  color: theme.text.primary,
  border: "none",
};

export const secondaryButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 18px",
  backgroundColor: theme.bg.secondaryButton,
  color: theme.text.secondaryButton,
  border: `1px solid ${theme.border.secondaryButton}`,
};

export const dangerOutlineButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 18px",
  backgroundColor: theme.bg.transparent,
  color: theme.text.danger,
  border: borderDanger,
};

export const successButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: theme.bg.successButton,
  color: theme.text.primary,
  border: "none",
};

export const warningButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: theme.bg.warningButton,
  color: theme.text.primary,
  border: "none",
};

export const indigoButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: theme.bg.indigoButton,
  color: theme.text.primary,
  border: "none",
};

export const dangerButton: CSSProperties = {
  ...buttonBase,
  padding: "10px 22px",
  backgroundColor: theme.bg.dangerButton,
  color: theme.text.primary,
  border: "none",
};

export const checkboxLabel: CSSProperties = {
  display: "flex",
  alignItems: "center",
  gap: "8px",
  color: theme.text.checkbox,
};

const preBlock: CSSProperties = {
  backgroundColor: theme.bg.code,
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

// Language selector styles

export const currentLanguageRow: CSSProperties = {
  marginTop: "12px",
  marginBottom: "12px",
};

export const clearLanguageButton: CSSProperties = {
  marginLeft: "12px",
  padding: "2px 10px",
  backgroundColor: theme.bg.transparent,
  color: theme.text.danger,
  border: borderDanger,
  borderRadius: "4px",
  cursor: "pointer",
  fontSize: "0.8rem",
};

export const languageListBox: CSSProperties = {
  maxHeight: "200px",
  overflowY: "auto",
  border: borderDefault,
  borderRadius: "6px",
  backgroundColor: theme.bg.input,
};

export const languageOptionBase: CSSProperties = {
  display: "block",
  width: "100%",
  textAlign: "left",
  padding: "8px 12px",
  border: "none",
  borderBottom: `1px solid ${theme.border.muted}`,
  cursor: "pointer",
  fontSize: "0.9rem",
};

export const languageIsoLabel: CSSProperties = {
  float: "right",
  color: theme.text.tertiary,
  fontSize: "0.8rem",
};
