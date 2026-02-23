import { beforeEach, describe, expect, it, vi } from "vitest";

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason?: unknown) => void;
};

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const store = new Map<string, string>();

const secureStoreMock = {
  getItemAsync: vi.fn<(key: string) => Promise<string | null>>(),
  setItemAsync: vi.fn<(key: string, value: string) => Promise<void>>(),
  deleteItemAsync: vi.fn<(key: string) => Promise<void>>(),
};

vi.mock("expo-secure-store", () => secureStoreMock);

function resetSecureStoreMocks(): void {
  store.clear();

  secureStoreMock.getItemAsync.mockReset();
  secureStoreMock.setItemAsync.mockReset();
  secureStoreMock.deleteItemAsync.mockReset();

  secureStoreMock.getItemAsync.mockImplementation(async (key: string) =>
    store.has(key) ? (store.get(key) ?? null) : null
  );
  secureStoreMock.setItemAsync.mockImplementation(async (key: string, value: string) => {
    store.set(key, value);
  });
  secureStoreMock.deleteItemAsync.mockImplementation(async (key: string) => {
    store.delete(key);
  });
}

beforeEach(() => {
  vi.resetModules();
  resetSecureStoreMocks();
});

describe("storage cache", () => {
  it("caches token after first SecureStore read", async () => {
    const storage = await import("./storage");
    store.set(storage.TOKEN_KEY, "token-1");

    await expect(storage.getToken()).resolves.toBe("token-1");
    await expect(storage.getToken()).resolves.toBe("token-1");
    expect(secureStoreMock.getItemAsync).toHaveBeenCalledTimes(1);
  });

  it("keeps clearToken result when clear races with in-flight getToken", async () => {
    const storage = await import("./storage");
    const pendingRead = deferred<string | null>();

    secureStoreMock.getItemAsync.mockImplementationOnce(() => pendingRead.promise);

    const inFlightGet = storage.getToken();
    await storage.clearToken();
    pendingRead.resolve("stale-token");

    await expect(inFlightGet).resolves.toBeNull();
    await expect(storage.getToken()).resolves.toBeNull();
  });

  it("returns default notification preferences when SecureStore read fails", async () => {
    const storage = await import("./storage");
    secureStoreMock.getItemAsync.mockRejectedValueOnce(new Error("read failed"));

    await expect(storage.getNotificationPreferences()).resolves.toEqual({
      completion: true,
      inputNeeded: true,
      error: true,
    });
  });

  it("maps notification preference values and caches the computed object", async () => {
    const storage = await import("./storage");

    secureStoreMock.getItemAsync
      .mockResolvedValueOnce("0")
      .mockResolvedValueOnce(null)
      .mockResolvedValueOnce("1");

    await expect(storage.getNotificationPreferences()).resolves.toEqual({
      completion: false,
      inputNeeded: true,
      error: true,
    });
    await expect(storage.getNotificationPreferences()).resolves.toEqual({
      completion: false,
      inputNeeded: true,
      error: true,
    });

    expect(secureStoreMock.getItemAsync).toHaveBeenCalledTimes(3);
  });

  it("keeps clearLanguage result when clear races with in-flight getLanguage", async () => {
    const storage = await import("./storage");
    const pendingRead = deferred<string | null>();

    secureStoreMock.getItemAsync.mockImplementationOnce(() => pendingRead.promise);

    const inFlightGet = storage.getLanguage();
    await storage.clearLanguage();
    pendingRead.resolve("pl-PL");

    await expect(inFlightGet).resolves.toBeNull();
    await expect(storage.getLanguage()).resolves.toBeNull();
  });
});
