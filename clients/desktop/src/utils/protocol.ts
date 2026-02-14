export const BINARY_MAGIC = 0xbf;
export const BINARY_FRAME_OUTPUT = 0x01;

export interface BinaryFrame {
  frameType: number;
  sessionId: string;
  payload: Uint8Array;
}

const textDecoder = new TextDecoder();

export function parseBinaryFrame(buffer: ArrayBuffer): BinaryFrame | null {
  if (buffer.byteLength < 4) {
    return null;
  }

  const view = new DataView(buffer);
  if (view.getUint8(0) !== BINARY_MAGIC) {
    return null;
  }

  const frameType = view.getUint8(1);
  const sessionIdLength = view.getUint16(2, true);
  const headerSize = 4;

  if (buffer.byteLength < headerSize + sessionIdLength) {
    return null;
  }

  let sessionId = '';
  try {
    const idBytes = new Uint8Array(buffer, headerSize, sessionIdLength);
    sessionId = textDecoder.decode(idBytes);
  } catch (err) {
    console.error('[WS] Failed to decode session id from binary frame', err);
    return null;
  }

  const payloadOffset = headerSize + sessionIdLength;
  if (payloadOffset > buffer.byteLength) {
    return null;
  }

  const payload = new Uint8Array(buffer, payloadOffset);

  return { frameType, sessionId, payload };
}
