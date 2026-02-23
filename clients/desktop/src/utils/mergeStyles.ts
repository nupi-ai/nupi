import type { CSSProperties } from "react";

type StyleInput = CSSProperties | undefined | null | false;

export function mergeStyles(...styles: StyleInput[]): CSSProperties {
  const merged: CSSProperties = {};
  for (const style of styles) {
    if (style) {
      Object.assign(merged, style);
    }
  }
  return merged;
}
