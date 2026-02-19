/**
 * Base64 encoding utility for transferring binary PTY output
 * from React Native to WebView via postMessage (string-only).
 * Decoding is handled inside the WebView bridge script.
 */

/** Convert an ArrayBuffer to a base64-encoded string. */
export function arrayBufferToBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  // Process in chunks to avoid call-stack limits and reduce
  // intermediate string allocations on large PTY buffers.
  const CHUNK = 8192;
  let binary = "";
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode(
      ...(bytes.subarray(i, i + CHUNK) as unknown as number[])
    );
  }
  return btoa(binary);
}

