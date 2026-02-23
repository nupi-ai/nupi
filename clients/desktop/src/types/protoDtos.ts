import type { LanguageInfo as ProtoLanguageInfo } from "../gen/daemon_pb";
import type { Recording as ProtoRecording } from "../gen/recordings_pb";
import type { Session as ProtoSession } from "../gen/sessions_pb";

// Desktop Tauri payloads use snake_case and RFC3339 timestamps.
// Field scalar types are sourced from generated proto types to keep
// contract drift localized to proto generation output.
export interface Session {
  id: ProtoSession["id"];
  command: ProtoSession["command"];
  args: ProtoSession["args"];
  status: ProtoSession["status"];
  pid?: ProtoSession["pid"];
  start_time?: string;
  exit_code?: NonNullable<ProtoSession["exitCode"]>;
  work_dir?: ProtoSession["workDir"];
  tool?: ProtoSession["tool"];
  tool_icon?: ProtoSession["toolIcon"];
  tool_icon_data?: ProtoSession["toolIconData"];
  mode?: ProtoSession["mode"];
}

export interface Recording {
  session_id: ProtoRecording["sessionId"];
  filename: ProtoRecording["filename"];
  command: ProtoRecording["command"];
  args: ProtoRecording["args"];
  work_dir?: ProtoRecording["workDir"];
  start_time?: string;
  duration: ProtoRecording["durationSec"];
  rows: ProtoRecording["rows"];
  cols: ProtoRecording["cols"];
  title: ProtoRecording["title"];
  tool?: ProtoRecording["tool"];
  recording_path: ProtoRecording["recordingPath"];
}

export interface LanguageInfo {
  iso1: ProtoLanguageInfo["iso1"];
  bcp47: ProtoLanguageInfo["bcp47"];
  english_name: ProtoLanguageInfo["englishName"];
  native_name: ProtoLanguageInfo["nativeName"];
}
