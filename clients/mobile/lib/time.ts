import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { timestampDate } from "@bufbuild/protobuf/wkt";

/**
 * Formats a protobuf Timestamp as a relative time string.
 * Returns "just now", "X min ago", "X hr ago", or "X days ago".
 */
export function formatRelativeTime(
  timestamp: Timestamp | undefined
): string {
  if (!timestamp) return "";

  const date = timestampDate(timestamp);
  const now = Date.now();
  const diffMs = now - date.getTime();

  if (diffMs < 0) return "just now";

  const seconds = Math.floor(diffMs / 1000);
  if (seconds < 60) return "just now";

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} hr ago`;

  const days = Math.floor(hours / 24);
  return `${days} day${days !== 1 ? "s" : ""} ago`;
}
