import type { CSSProperties } from "react";

export const root: CSSProperties = {
  display: "flex",
  flexDirection: "column",
  flex: 1,
  minHeight: 0,
  overflow: "hidden",
};

export const terminalContainer: CSSProperties = {
  flex: 1,
  backgroundColor: "#262626",
  padding: "8px 8px 0 8px",
  boxSizing: "border-box",
  minHeight: 0,
  display: "flex",
  position: "relative",
};

export const terminalViewport: CSSProperties = {
  flex: 1,
  minHeight: 0,
  overflowX: "auto",
  overflowY: "hidden",
  paddingRight: "24px",
};

export const customScrollbar: CSSProperties = {
  position: "absolute",
  top: "8px",
  right: "0",
  bottom: "8px",
  width: "16px",
  overflowY: "auto",
  overflowX: "hidden",
  visibility: "hidden",
  pointerEvents: "none",
  backgroundColor: "rgba(38, 38, 38, 0.4)",
  borderRadius: "6px 0 0 6px",
  willChange: "scroll-position",
  scrollBehavior: "auto",
};

export const customScrollbarContent: CSSProperties = {
  width: "100%",
  background: "transparent",
};
