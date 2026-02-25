import { existsSync, readFileSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { sharedDesignTokens } from "@nupi/shared/design-tokens";

import Colors from "./designTokens";

describe("designTokens snapshot", () => {
  it("keeps mobile light palette stable", () => {
    expect(Colors.light).toEqual({
      text: "#000",
      background: "#fff",
      tint: "#1977b5",
      danger: "#dc2626",
      success: "#22c55e",
      warning: "#eab308",
      onWarning: "#000",
      onDanger: "#fff",
      surface: "#f0f0f0",
      separator: "#ddd",
      overlay: "rgba(0,0,0,0.5)",
      onOverlay: "#fff",
      terminalBackground: "#262626",
    });
  });

  it("keeps mobile dark palette stable", () => {
    expect(Colors.dark).toEqual({
      text: "#fff",
      background: "#000",
      tint: "#fff",
      danger: "#ff4444",
      success: "#22c55e",
      warning: "#eab308",
      onWarning: "#000",
      onDanger: "#fff",
      surface: "#1c1c1e",
      separator: "#333",
      overlay: "rgba(0,0,0,0.7)",
      onOverlay: "#fff",
      terminalBackground: "#262626",
    });
  });
});

describe("cross-client shared token wiring", () => {
  it("reuses shared primitives in both clients for common colors", () => {
    expect(Colors.dark.separator).toBe(sharedDesignTokens.color.neutral333);
    expect(Colors.dark.terminalBackground).toBe(sharedDesignTokens.color.terminalBackground);

    const desktopTokensPath = resolve(
      dirname(fileURLToPath(import.meta.url)),
      "../../desktop/src/designTokens.ts",
    );
    if (!existsSync(desktopTokensPath)) {
      // Desktop source not available (mobile-only CI/checkout) — skip cross-client file assertions
      return;
    }
    const desktopTokensSource = readFileSync(desktopTokensPath, "utf8");
    expect(desktopTokensSource).toContain("sharedDesignTokens.color.white");
    expect(desktopTokensSource).toContain("sharedDesignTokens.color.neutral333");
    expect(desktopTokensSource).toContain("sharedDesignTokens.overlay.black70");
  });
});
