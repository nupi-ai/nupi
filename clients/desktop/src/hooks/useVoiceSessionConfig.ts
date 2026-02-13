import { useMemo, useState } from "react";

export interface VoiceSessionConfig {
  sessionId: string;
  setSessionId: (value: string) => void;
  streamId: string;
  setStreamId: (value: string) => void;
  metadata: Record<string, string>;
}

export function useVoiceSessionConfig(): VoiceSessionConfig {
  const [sessionId, setSessionId] = useState("");
  const [streamId, setStreamId] = useState("mic");
  const metadata = useMemo(() => ({ client: "desktop" }), []);

  return { sessionId, setSessionId, streamId, setStreamId, metadata };
}
