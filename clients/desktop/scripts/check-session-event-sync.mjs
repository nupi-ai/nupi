#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const sessionProtoPath = path.resolve(__dirname, '../../../api/grpc/v1/sessions.proto');
const frontendPath = path.resolve(__dirname, '../src/lib/sessionEvents.ts');
const tauriPath = path.resolve(__dirname, '../src-tauri/src/lib.rs');

const sessionProto = fs.readFileSync(sessionProtoPath, 'utf8');
const frontend = fs.readFileSync(frontendPath, 'utf8');
const tauri = fs.readFileSync(tauriPath, 'utf8');

const enumMatch = sessionProto.match(/enum\s+SessionEventType\s*\{([\s\S]*?)\n\}/);
if (!enumMatch) {
  console.error('Cannot find enum SessionEventType in sessions.proto');
  process.exit(1);
}

const protoValues = Array.from(
  enumMatch[1].matchAll(/\bSESSION_EVENT_TYPE_([A-Z_]+)\s*=\s*\d+\s*;/g),
  (m) => m[1]
).filter((name) => name !== 'UNSPECIFIED');

const protoToFrontend = {
  CREATED: 'session_created',
  KILLED: 'session_killed',
  STATUS_CHANGED: 'session_status_changed',
  MODE_CHANGED: 'session_mode_changed',
  TOOL_DETECTED: 'tool_detected',
  RESIZE_INSTRUCTION: 'resize_instruction',
};

const expectedFrontendValues = protoValues.map((name) => {
  const mapped = protoToFrontend[name];
  if (!mapped) {
    throw new Error(`Missing mapping for proto enum value SESSION_EVENT_TYPE_${name}`);
  }
  return mapped;
});

const listMatch = frontend.match(/export const SESSION_EVENT_TYPES = \[([\s\S]*?)\] as const;/);
if (!listMatch) {
  console.error('Cannot find SESSION_EVENT_TYPES in sessionEvents.ts');
  process.exit(1);
}

const frontendValues = Array.from(listMatch[1].matchAll(/'([^']+)'/g), (m) => m[1]);

const sortedExpected = [...expectedFrontendValues].sort();
const sortedFrontend = [...frontendValues].sort();
const sameLength = sortedFrontend.length === sortedExpected.length;
const sameItems = sameLength && sortedFrontend.every((value, index) => value === sortedExpected[index]);

if (!sameItems) {
  console.error('Session event types are out of sync with proto SessionEventType.');
  console.error('Expected:', expectedFrontendValues.join(', '));
  console.error('Found:   ', frontendValues.join(', '));
  process.exit(1);
}

const mappingFn = tauri.match(/fn\s+session_event_type_to_wire\s*\([^)]*\)\s*->\s*&'static\s+str\s*\{([\s\S]*?)\n\}/);
if (!mappingFn) {
  console.error('Cannot find session_event_type_to_wire in src-tauri/src/lib.rs');
  process.exit(1);
}

const tauriMappings = Array.from(
  mappingFn[1].matchAll(/SessionEventType::([A-Za-z]+)\s*=>\s*"([^"]+)"/g),
  (m) => ({ key: m[1], value: m[2] })
);

const tauriByKey = Object.fromEntries(tauriMappings.map((entry) => [entry.key, entry.value]));

const protoToTauriKey = {
  CREATED: 'Created',
  KILLED: 'Killed',
  STATUS_CHANGED: 'StatusChanged',
  MODE_CHANGED: 'ModeChanged',
  TOOL_DETECTED: 'ToolDetected',
  RESIZE_INSTRUCTION: 'ResizeInstruction',
};

for (const protoName of protoValues) {
  const tauriKey = protoToTauriKey[protoName];
  if (!tauriKey) {
    console.error(`Missing proto->tauri key mapping for SESSION_EVENT_TYPE_${protoName}`);
    process.exit(1);
  }
  const tauriWireValue = tauriByKey[tauriKey];
  const expectedWireValue = protoToFrontend[protoName];
  if (tauriWireValue !== expectedWireValue) {
    console.error(`Tauri mapping mismatch for ${tauriKey}.`);
    console.error(`Expected: ${expectedWireValue}`);
    console.error(`Found:    ${tauriWireValue ?? '<missing>'}`);
    process.exit(1);
  }
}

console.log('Session event type sync check passed.');
