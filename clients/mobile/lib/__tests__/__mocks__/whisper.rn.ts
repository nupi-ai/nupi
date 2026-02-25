/**
 * Minimal whisper.rn stub for vitest tests.
 * whisper.rn has a broken "exports" field in its package.json (missing "."
 * specifier), so Vite's resolver fails before vi.mock can intercept.
 * This alias resolves the package to a stub that tests can spy on.
 */
import { vi } from "vitest";

export const initWhisper = vi.fn(async (opts: any) => ({
  release: vi.fn(async () => {}),
  transcribe: vi.fn(async () => ({ result: "", segments: [] })),
}));

export type WhisperContext = Awaited<ReturnType<typeof initWhisper>>;
