// @vitest-environment jsdom
/**
 * LAN-Only Operation Validation Tests (Story 11-5, Task 4: AC3)
 *
 * Validates that the mobile app operates fully on-LAN without internet:
 * 4.1 — Whisper STT has zero network calls after model download
 * 4.2 — Connect RPC client uses daemon host from pairing config (LAN)
 * 4.3 — Audio WebSocket URL constructed from ConnectionContext host (LAN)
 * 4.4/4.5 — Push notification graceful degradation (voice unaffected)
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";

// ---------------------------------------------------------------------------
// Top-level mocks (hoisted by vitest before imports)
//
// whisper.rn is resolved via vitest.config.ts alias to __mocks__/whisper.rn.ts
// react-native is resolved via vitest.config.ts alias to __mocks__/react-native.ts
// No vi.mock needed for those — Vite aliases handle them.
// ---------------------------------------------------------------------------

vi.mock("expo-file-system", () => {
  class MockFile {
    uri: string;
    exists = true;
    size = 600 * 1024 * 1024;
    constructor(dirOrPath: any, name: string) {
      const base = typeof dirOrPath === "string"
        ? dirOrPath
        : dirOrPath?.uri ?? dirOrPath?.path ?? "/mock/documents/models";
      this.uri = base.startsWith("file://") ? `${base}/${name}` : `file://${base}/${name}`;
    }
    delete() {}
  }
  class MockDirectory {
    uri: string;
    exists = true;
    path: string;
    constructor(parent: any, name: string) {
      const base = typeof parent === "string"
        ? parent
        : parent?.uri ?? parent?.path ?? "/mock/documents";
      this.uri = `${base}/${name}`;
      this.path = this.uri;
    }
    create() {}
    delete() {}
  }
  return {
    Paths: { document: "/mock/documents" },
    File: MockFile,
    Directory: MockDirectory,
  };
});

vi.mock("expo-file-system/legacy", () => ({
  createDownloadResumable: vi.fn(),
}));

vi.mock("react-native-zip-archive", () => ({
  unzip: vi.fn(),
}));

vi.mock("expo-secure-store", () => ({
  getItemAsync: vi.fn(async (key: string) => {
    if (key === "nupi_device_id") return "mock-device-id";
    return null;
  }),
  setItemAsync: vi.fn(async () => {}),
  deleteItemAsync: vi.fn(async () => {}),
}));

// Connect RPC mocks
let capturedTransportOpts: any = null;
vi.mock("@connectrpc/connect", () => ({
  createClient: vi.fn(() => new Proxy({}, { get: () => vi.fn() })),
}));
vi.mock("@connectrpc/connect-web", () => ({
  createConnectTransport: vi.fn((opts: any) => {
    capturedTransportOpts = opts;
    return { baseUrl: opts.baseUrl };
  }),
}));
vi.mock("@/lib/gen/nupi_pb", () => ({
  DaemonService: {},
  SessionsService: {},
  AuthService: {},
}));

// Notification mocks
vi.mock("expo-notifications", () => ({
  getPermissionsAsync: vi.fn(async () => ({ status: "granted" })),
  requestPermissionsAsync: vi.fn(async () => ({ status: "granted" })),
  getExpoPushTokenAsync: vi.fn(async () => {
    throw new Error("Network unreachable — no internet");
  }),
  setNotificationHandler: vi.fn(),
  setNotificationChannelAsync: vi.fn(async () => {}),
  addPushTokenListener: vi.fn(() => ({ remove: vi.fn() })),
  addNotificationReceivedListener: vi.fn(() => ({ remove: vi.fn() })),
  addNotificationResponseReceivedListener: vi.fn(() => ({ remove: vi.fn() })),
  getLastNotificationResponseAsync: vi.fn(async () => null),
  AndroidImportance: { HIGH: 4 },
}));
vi.mock("expo-device", () => ({
  isDevice: true,
}));
vi.mock("expo-crypto", () => ({
  randomUUID: () => "mock-uuid-1234-5678",
}));
vi.mock("expo-constants", () => ({
  default: {
    expoConfig: { extra: { eas: { projectId: "test-project-id" } } },
  },
}));
vi.mock("../gen/auth_pb", () => ({
  NotificationEventType: {
    TASK_COMPLETED: 1,
    INPUT_NEEDED: 2,
    ERROR: 3,
  },
}));

// Mock storage for connect.ts interceptors (getToken, getLanguage) and
// useAudioWebSocket (getToken, getConnectionConfig).
vi.mock("../storage", () => ({
  getToken: vi.fn(async () => "mock-token"),
  getLanguage: vi.fn(async () => null),
  getConnectionConfig: vi.fn(async () => ({
    host: "192.168.1.42",
    port: 9090,
    tls: false,
  })),
  getNotificationPreferences: vi.fn(async () => ({
    completion: true,
    inputNeeded: true,
    error: true,
  })),
}));

// ---------------------------------------------------------------------------
// Imports (after mocks)
// ---------------------------------------------------------------------------

import { whisperManager, isModelDownloaded, isCoreMLDownloaded } from "../whisper";
import { initWhisper } from "whisper.rn";
import {
  createNupiClient,
  createNupiClientFromConfig,
} from "../connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { useAudioWebSocket } from "../useAudioWebSocket";
import { getToken, getConnectionConfig } from "../storage";
import {
  registerPushToken,
  getExpoPushToken,
  cancelInflightRegistration,
} from "../notifications";
import * as ExpoNotifications from "expo-notifications";
import * as ExpoDevice from "expo-device";
import ExpoConstants from "expo-constants";
import { renderHook, act, cleanup } from "@testing-library/react";
import { createMockAuthClient } from "./testHelpers";

// ---------------------------------------------------------------------------
// 4.1 — Whisper STT: zero network calls during model init & transcription
// ---------------------------------------------------------------------------

describe("4.1 — Whisper STT zero network calls (post-download)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("initWhisperContext uses local filePath, on-device flags, and makes zero network calls", async () => {
    // Combined test: verifies local path, device-specific flags, AND no network calls
    // in a single init() invocation. Avoids singleton cache issues between separate tests.

    // Instrument global network APIs BEFORE init to detect any calls
    const fetchSpy = vi.fn();
    const origFetch = globalThis.fetch;
    globalThis.fetch = fetchSpy as any;

    const xhrInstances: unknown[] = [];
    const origXHR = (globalThis as any).XMLHttpRequest;
    (globalThis as any).XMLHttpRequest = function MockXHR() {
      xhrInstances.push(this);
    };

    try {
      expect(isModelDownloaded()).toBe(true);

      await whisperManager.init();

      // Verify local path and device flags
      expect(initWhisper).toHaveBeenCalledTimes(1);
      const callArgs = vi.mocked(initWhisper).mock.calls[0][0];

      // filePath must be a local filesystem path, not an HTTP URL
      expect(callArgs.filePath).toBeDefined();
      expect(callArgs.filePath).not.toMatch(/^https?:\/\//);
      // Should contain the model directory and filename
      expect(callArgs.filePath).toMatch(/models/);
      expect(callArgs.filePath).toMatch(/ggml-medium/);
      // Must not reference any internet host
      expect(callArgs.filePath).not.toContain("huggingface");
      expect(callArgs.filePath).not.toContain("amazonaws");

      // On-device inference: GPU disabled (whisper.rn uses CPU+CoreML), CoreML enabled for iOS
      expect(callArgs.useGpu).toBe(false);
      expect(callArgs.useCoreMLIos).toBe(true);

      // Verify zero network calls during init
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(xhrInstances).toHaveLength(0);
    } finally {
      globalThis.fetch = origFetch;
      (globalThis as any).XMLHttpRequest = origXHR;
    }
  });

  it("MODEL_URL and COREML_URL are module-private (not exported)", async () => {
    // Runtime check: verify download URLs are NOT accessible via module exports.
    // This ensures internal constants can't be imported by other modules.
    const whisperExports = await import("../whisper");

    expect(whisperExports).not.toHaveProperty("MODEL_URL");
    expect(whisperExports).not.toHaveProperty("COREML_URL");

    // Verify expected public API IS exported
    expect(whisperExports).toHaveProperty("whisperManager");
    expect(whisperExports).toHaveProperty("isModelDownloaded");
    expect(whisperExports).toHaveProperty("isCoreMLDownloaded");
  });
});

// ---------------------------------------------------------------------------
// 4.2 — Connect RPC: uses daemon host from pairing config (LAN address)
// ---------------------------------------------------------------------------

describe("4.2 — Connect RPC uses LAN config, no hardcoded internet URLs", () => {
  beforeEach(() => {
    capturedTransportOpts = null;
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("buildBaseUrl constructs http:// URL from config host/port when tls=false", () => {
    const config = { host: "192.168.1.42", port: 9090, tls: false };
    createNupiClientFromConfig(config);

    expect(createConnectTransport).toHaveBeenCalledWith(
      expect.objectContaining({
        baseUrl: "http://192.168.1.42:9090",
      }),
    );
  });

  it("buildBaseUrl constructs https:// URL from config host/port when tls=true", () => {
    const config = { host: "10.0.0.5", port: 443, tls: true };
    createNupiClientFromConfig(config);

    expect(createConnectTransport).toHaveBeenCalledWith(
      expect.objectContaining({
        baseUrl: "https://10.0.0.5:443",
      }),
    );
  });

  it("createNupiClient passes the exact baseUrl string to transport (no mutations)", () => {
    createNupiClient("http://my-lan-host.local:8080");

    expect(capturedTransportOpts).toBeDefined();
    expect(capturedTransportOpts.baseUrl).toBe("http://my-lan-host.local:8080");
  });

  it("supports .local mDNS hostnames for LAN discovery", () => {
    const config = { host: "nupi-daemon.local", port: 9090, tls: false };
    createNupiClientFromConfig(config);

    expect(createConnectTransport).toHaveBeenCalledWith(
      expect.objectContaining({
        baseUrl: "http://nupi-daemon.local:9090",
      }),
    );
  });

  it("transport includes auth and language interceptors for LAN RPC calls", () => {
    createNupiClient("http://192.168.1.1:9090");

    expect(capturedTransportOpts.interceptors).toBeDefined();
    expect(capturedTransportOpts.interceptors).toHaveLength(2);
    expect(capturedTransportOpts.useBinaryFormat).toBe(true);
  });

  it("connect.ts has no hardcoded internet URLs — only dynamic config-based construction", async () => {
    // Static analysis: verify no hardcoded internet URLs exist in source.
    // Runtime tests above validate config→URL mapping; source inspection
    // is needed to prove absence of fallback/hardcoded URLs.
    // FRAGILE: Source-reading tests break on file renames/moves. Replace with
    // dependency-cruiser or lint rule when CI infrastructure is available.
    const fs = await import("fs");
    const path = await import("path");
    const filePath = path.resolve(__dirname, "../connect.ts");
    expect(fs.existsSync(filePath), `Source file not found: ${filePath}`).toBe(true);
    const source = fs.readFileSync(filePath, "utf-8");
    // No hardcoded http(s) URLs
    const urlMatches = source.match(/["'`]https?:\/\/[^"'`]+["'`]/g) ?? [];
    expect(urlMatches).toHaveLength(0);
    // buildBaseUrl uses template literal with config values
    expect(source).toMatch(/\$\{protocol\}:\/\/\$\{config\.host\}/);
  });
});

// ---------------------------------------------------------------------------
// 4.3 — Audio WebSocket URL from ConnectionContext host (LAN)
// ---------------------------------------------------------------------------

describe("4.3 — Audio WebSocket URL uses LAN config host", () => {
  let wsInstances: Array<{ url: string; binaryType: string }>;

  beforeEach(() => {
    wsInstances = [];

    // Install MockWebSocket globally — simulates successful LAN connection
    class MockWS {
      static OPEN = 1;
      static CONNECTING = 0;
      static CLOSING = 2;
      static CLOSED = 3;
      url: string;
      binaryType = "blob";
      readyState = 0;
      onopen: any = null;
      onmessage: any = null;
      onerror: any = null;
      onclose: any = null;
      constructor(url: string) {
        this.url = url;
        wsInstances.push({ url, binaryType: this.binaryType });
        // Simulate async connection success (LAN is fast, fires on next microtask)
        queueMicrotask(() => {
          this.readyState = MockWS.OPEN;
          if (this.onopen) this.onopen(new Event("open"));
        });
      }
      send() {}
      close() { this.readyState = MockWS.CLOSED; }
    }
    vi.stubGlobal("WebSocket", MockWS);
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  /** Flush microtasks for openWebSocket's async getToken/getConnectionConfig. */
  async function flushAsync() {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  }

  it("WebSocket URL uses ws:// scheme with config host/port when tls=false", async () => {
    vi.mocked(getToken).mockResolvedValue("my-lan-token");
    vi.mocked(getConnectionConfig).mockResolvedValue({
      host: "192.168.1.42",
      port: 9090,
      tls: false,
    });

    const { result } = renderHook(() =>
      useAudioWebSocket("session-42", {}),
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    expect(wsInstances.length).toBe(1);
    const url = wsInstances[0].url;

    expect(url).toMatch(/^ws:\/\/192\.168\.1\.42:9090/);
    expect(url).toContain("/ws/audio/session-42");
    expect(url).toContain("token=my-lan-token");
    // Must NOT contain any internet domain
    expect(url).not.toMatch(/nupi\.io|nupi\.com|amazonaws|cloudfront/);
  });

  it("WebSocket URL uses wss:// scheme with config host/port when tls=true", async () => {
    vi.mocked(getToken).mockResolvedValue("secure-token");
    vi.mocked(getConnectionConfig).mockResolvedValue({
      host: "10.0.0.5",
      port: 443,
      tls: true,
    });

    const { result } = renderHook(() =>
      useAudioWebSocket("session-99", {}),
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    expect(wsInstances.length).toBe(1);
    const url = wsInstances[0].url;

    expect(url).toMatch(/^wss:\/\/10\.0\.0\.5:443/);
    expect(url).toContain("/ws/audio/session-99");
    expect(url).toContain("token=secure-token");
  });

  it("WebSocket URL token is properly URI-encoded", async () => {
    vi.mocked(getToken).mockResolvedValue("token/with+special=chars&more");
    vi.mocked(getConnectionConfig).mockResolvedValue({
      host: "192.168.0.1",
      port: 8080,
      tls: false,
    });

    const { result } = renderHook(() =>
      useAudioWebSocket("s1", {}),
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    expect(wsInstances.length).toBe(1);
    const url = wsInstances[0].url;

    // Token must be URI-encoded (encodeURIComponent)
    expect(url).toContain("token=token%2Fwith%2Bspecial%3Dchars%26more");
  });

  it("WebSocket URL contains session ID from parameter, not hardcoded", async () => {
    vi.mocked(getToken).mockResolvedValue("t");
    vi.mocked(getConnectionConfig).mockResolvedValue({
      host: "192.168.1.1",
      port: 9090,
      tls: false,
    });

    const { result } = renderHook(() =>
      useAudioWebSocket("my-unique-session-id", {}),
    );

    await act(async () => {
      result.current.connect();
      await flushAsync();
    });

    expect(wsInstances.length).toBe(1);
    expect(wsInstances[0].url).toContain("/ws/audio/my-unique-session-id");
  });
});

// ---------------------------------------------------------------------------
// 4.4 & 4.5 — Push notification graceful degradation
// ---------------------------------------------------------------------------

describe("4.4/4.5 — Push notification failure does not block voice pipeline", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Reset the in-flight registration guard between tests
    cancelInflightRegistration();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("registerPushToken returns false when getExpoPushToken fails (graceful degradation)", async () => {
    // getExpoPushTokenAsync is mocked to throw at the top level.
    // registerPushToken should catch this and return false, not throw.
    const mockClient = createMockAuthClient();

    const result = await registerPushToken(mockClient);

    // Should return false (failure) but NOT throw
    expect(result).toBe(false);
    // RPC should NOT have been called since token acquisition failed
    expect(mockClient.auth.registerPushToken).not.toHaveBeenCalled();
  });

  it("registerPushToken returns null (skipped) on non-device/simulator", async () => {
    // Temporarily make isDevice return false
    const origIsDevice = ExpoDevice.isDevice;
    Object.defineProperty(ExpoDevice, "isDevice", { value: false, writable: true });

    const mockClient = createMockAuthClient();

    const result = await registerPushToken(mockClient);

    // null = "skipped, not failed" — voice pipeline is unaffected
    expect(result).toBe(null);
    expect(mockClient.auth.registerPushToken).not.toHaveBeenCalled();

    // Restore
    Object.defineProperty(ExpoDevice, "isDevice", { value: origIsDevice, writable: true });
  });

  it("registerPushToken RPC failure returns false without throwing (voice continues)", async () => {
    // Make getExpoPushTokenAsync succeed for this test
    vi.mocked(ExpoNotifications.getExpoPushTokenAsync).mockResolvedValueOnce({
      data: "ExponentPushToken[valid-token]",
      type: "expo",
    });

    const mockClient = createMockAuthClient({
      registerPushToken: vi.fn(async () => {
        throw new Error("connect ECONNREFUSED 192.168.1.42:9090");
      }),
    });

    // Should NOT throw — returns false (failure) gracefully
    const result = await registerPushToken(mockClient);
    expect(result).toBe(false);
  });

  it("getExpoPushToken returns null when EAS projectId is a placeholder", async () => {
    // Override Constants to have a placeholder projectId
    const origConfig = ExpoConstants.expoConfig;
    (ExpoConstants as any).expoConfig = {
      extra: { eas: { projectId: "YOUR_PROJECT_ID" } },
    };

    // Suppress __DEV__ throw — test production behavior
    const origDev = (globalThis as any).__DEV__;
    (globalThis as any).__DEV__ = false;

    const token = await getExpoPushToken();
    expect(token).toBeNull();

    // Restore
    (ExpoConstants as any).expoConfig = origConfig;
    (globalThis as any).__DEV__ = origDev;
  });

  it("getExpoPushToken returns null on simulator (isDevice=false)", async () => {
    const origIsDevice = ExpoDevice.isDevice;
    Object.defineProperty(ExpoDevice, "isDevice", { value: false, writable: true });

    const token = await getExpoPushToken();
    expect(token).toBeNull();

    Object.defineProperty(ExpoDevice, "isDevice", { value: origIsDevice, writable: true });
  });

  it("notification subsystem is architecturally isolated from voice pipeline", async () => {
    // Static analysis: verify no cross-imports between notification and voice/audio subsystems.
    // This architectural boundary ensures notification failures can't affect voice pipeline.
    // FRAGILE: Source-reading approach breaks on file renames/moves. Replace with
    // dependency-cruiser or lint rule when CI infrastructure is available.
    const fs = await import("fs");
    const path = await import("path");
    const libDir = path.resolve(__dirname, "..");

    const sourceFiles = ["notifications.ts", "whisper.ts", "useAudioWebSocket.ts"];
    for (const file of sourceFiles) {
      const filePath = path.join(libDir, file);
      expect(fs.existsSync(filePath), `Source file not found: ${filePath}`).toBe(true);
    }

    const notifSource = fs.readFileSync(path.join(libDir, "notifications.ts"), "utf-8");
    const whisperSource = fs.readFileSync(path.join(libDir, "whisper.ts"), "utf-8");
    const audioWsSource = fs.readFileSync(path.join(libDir, "useAudioWebSocket.ts"), "utf-8");

    // notifications.ts must NOT import voice/audio modules
    expect(notifSource).not.toMatch(/from\s+["']\.\/whisper["']/);
    expect(notifSource).not.toMatch(/from\s+["']\.\/useAudioWebSocket["']/);
    expect(notifSource).not.toMatch(/from\s+["']\.\/useAudioPlayback["']/);
    expect(notifSource).not.toMatch(/from\s+["']\.\/audioProtocol["']/);

    // whisper.ts must NOT import notification modules
    expect(whisperSource).not.toMatch(/from\s+["']\.\/notifications["']/);
    expect(whisperSource).not.toMatch(/expo-notifications/);

    // useAudioWebSocket.ts must NOT import notification modules
    expect(audioWsSource).not.toMatch(/from\s+["']\.\/notifications["']/);
    expect(audioWsSource).not.toMatch(/expo-notifications/);
  });
});
