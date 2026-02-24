import { ConnectError } from "@connectrpc/connect";
import { connectCodeToErrorCode, ErrorCode } from "@nupi/shared/errors";

import { createNupiClient } from "./connect";
import type { MappedError } from "./errorMessages";
import type { StoredConnectionConfig } from "./storage";

/**
 * Parses a nupi://pair?code=...&host=...&port=...&tls=... URL
 * from a scanned QR code. Returns null if the URL is invalid.
 */
export function parseNupiPairUrl(
  rawUrl: string
): { code: string; host: string; port: number; tls: boolean } | null {
  try {
    const url = new URL(rawUrl);
    if (url.protocol !== "nupi:") return null;
    if (url.hostname !== "pair") return null;

    const code = url.searchParams.get("code");
    const host = url.searchParams.get("host");
    const portStr = url.searchParams.get("port");

    if (!code || !host || !portStr) return null;

    // Validate host to prevent SSRF via userinfo/path injection
    const probe = new URL(`http://${host}`);
    if (probe.hostname !== host || probe.username || probe.pathname !== "/") {
      return null;
    }

    const port = parseInt(portStr, 10);
    if (isNaN(port) || port <= 0 || port > 65535) return null;

    const tls = url.searchParams.get("tls") === "true";

    return { code, host, port, tls };
  } catch {
    return null;
  }
}

/**
 * Claims a pairing code via Connect RPC. Creates a temporary client
 * to the QR code's host:port, calls ClaimPairing, and returns the
 * token and connection config.
 *
 * Does NOT store credentials — caller (ConnectionContext) handles storage.
 */
export async function claimPairing(params: {
  code: string;
  host: string;
  port: number;
  tls: boolean;
  clientName: string;
}): Promise<{ token: string; config: StoredConnectionConfig }> {
  const protocol = params.tls ? "https" : "http";
  const baseUrl = `${protocol}://${params.host}:${params.port}`;
  const tempClient = createNupiClient(baseUrl);

  const response = await tempClient.auth.claimPairing({
    code: params.code,
    clientName: params.clientName,
  });

  if (!response.token) {
    throw new Error("Server returned empty authentication token");
  }

  return {
    token: response.token,
    config: {
      host: params.host,
      port: params.port,
      tls: params.tls,
    },
  };
}

/**
 * Maps ClaimPairing errors to user-facing messages.
 */
export function mapPairingError(err: unknown): MappedError {
  if (err instanceof ConnectError) {
    switch (connectCodeToErrorCode(err.code)) {
      case ErrorCode.FailedPrecondition:
        return {
          message: "Pairing code expired. Generate a new one on your desktop.",
          action: "re-pair",
          canRetry: false,
        };
      case ErrorCode.NotFound:
        return {
          message: "Invalid pairing code.",
          action: "re-pair",
          canRetry: false,
        };
      case ErrorCode.Unavailable:
        return {
          message: "Cannot reach nupid. Check your network connection.",
          action: "retry",
          canRetry: true,
        };
      default:
        return {
          message: `Pairing failed: ${err.message}`,
          action: "retry",
          canRetry: true,
        };
    }
  }
  if (err instanceof Error) {
    return {
      message: `Connection error: ${err.message}`,
      action: "retry",
      canRetry: true,
    };
  }
  return {
    message: "An unexpected error occurred.",
    action: "retry",
    canRetry: true,
  };
}
