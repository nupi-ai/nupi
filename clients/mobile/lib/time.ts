import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { formatRelativeTimeFromDate } from "@nupi/shared/time";

/**
 * Formats a protobuf Timestamp as a relative time string.
 * Returns "just now", "X min ago", "X hr ago", or "X days ago".
 */
export function formatRelativeTime(
  timestamp: Timestamp | undefined
): string {
  if (!timestamp) return "";
  return formatRelativeTimeFromDate(timestampDate(timestamp));
}
