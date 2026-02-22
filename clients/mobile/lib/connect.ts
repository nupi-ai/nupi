import { createClient, type Client, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import {
  DaemonService,
  SessionsService,
  AuthService,
} from "@/lib/gen/nupi_pb";
import { getToken, getLanguage } from "./storage";

export interface ConnectionConfig {
  host: string;
  port: number;
  tls: boolean;
}

/**
 * Interceptor that attaches Bearer token to every outgoing Connect RPC request.
 * M2 fix: reads from in-memory cache (populated by storage.ts) to avoid
 * repeated SecureStore disk reads during fast polling intervals.
 */
const authInterceptor: Interceptor = (next) => async (req) => {
  try {
    const token = await getToken();
    if (token) {
      req.header.set("Authorization", `Bearer ${token}`);
    }
  } catch {
    // Proceed unauthenticated — server will return proper Unauthenticated error.
  }
  return next(req);
};

/**
 * Interceptor that attaches nupi-language header from stored preference.
 * AC1 requires voice commands to include language preference when set.
 * M2 fix: reads from in-memory cache to avoid SecureStore reads per RPC.
 */
const languageInterceptor: Interceptor = (next) => async (req) => {
  try {
    const lang = await getLanguage();
    if (lang) {
      req.header.set("nupi-language", lang);
    }
  } catch {
    // Proceed without language header — server gracefully handles missing header.
  }
  return next(req);
};

function buildBaseUrl(config: Pick<ConnectionConfig, "host" | "port" | "tls">) {
  const protocol = config.tls ? "https" : "http";
  return `${protocol}://${config.host}:${config.port}`;
}

/**
 * Mobile-safe subset of SessionsService.
 * Excludes createSession (FR63), killSession, sendInput, setSessionMode
 * per Boundary 6: mobile clients are stateless viewers only.
 */
type MobileSessionsClient = Pick<
  Client<typeof SessionsService>,
  | "listSessions"
  | "getSession"
  | "getSessionMode"
  | "getConversation"
  | "getGlobalConversation"
  | "sendVoiceCommand"
>;

/**
 * Mobile-safe subset of AuthService.
 * Only exposes claimPairing (pairing flow) and read-only token listing.
 * Excludes createToken, deleteToken, createPairing (admin operations).
 */
type MobileAuthClient = Pick<
  Client<typeof AuthService>,
  "claimPairing" | "listTokens" | "listPairings"
>;

/**
 * Creates typed Connect RPC clients for Nupi daemon services.
 * Client methods are restricted to the mobile-safe subset per architecture:
 * - Sessions: read-only viewer (no createSession, killSession, sendInput)
 * - Auth: claimPairing + read-only (no createToken, deleteToken, createPairing)
 * - Daemon: full access (includes Shutdown/ReloadPlugins — server-side RBAC enforces access control)
 */
export function createNupiClient(baseUrl: string) {
  const transport = createConnectTransport({
    baseUrl,
    useBinaryFormat: true,
    interceptors: [authInterceptor, languageInterceptor],
  });

  const fullSessions = createClient(SessionsService, transport);
  const fullAuth = createClient(AuthService, transport);

  const sessions: MobileSessionsClient = {
    listSessions: fullSessions.listSessions.bind(fullSessions),
    getSession: fullSessions.getSession.bind(fullSessions),
    getSessionMode: fullSessions.getSessionMode.bind(fullSessions),
    getConversation: fullSessions.getConversation.bind(fullSessions),
    getGlobalConversation: fullSessions.getGlobalConversation.bind(fullSessions),
    sendVoiceCommand: fullSessions.sendVoiceCommand.bind(fullSessions),
  };

  const auth: MobileAuthClient = {
    claimPairing: fullAuth.claimPairing.bind(fullAuth),
    listTokens: fullAuth.listTokens.bind(fullAuth),
    listPairings: fullAuth.listPairings.bind(fullAuth),
  };

  return {
    daemon: createClient(DaemonService, transport),
    sessions,
    auth,
  };
}

/**
 * Creates a Nupi client from a ConnectionConfig (e.g. parsed from QR code).
 */
export function createNupiClientFromConfig(
  config: Pick<ConnectionConfig, "host" | "port" | "tls">
) {
  return createNupiClient(buildBaseUrl(config));
}

export type NupiClient = ReturnType<typeof createNupiClient>;
