import type { Session } from '../types/session';

// Check if session is active (process is alive - running or detached)
// For Tauri UI, we treat both 'running' and 'detached' as active
// because attaching to a detached session makes it running anyway
export function isSessionActive(session: Session): boolean {
  return session.status === 'running' || session.status === 'detached';
}

// Truncate path showing the end with ellipsis at the beginning
export function truncatePath(path: string | undefined, maxLength: number = 40): string {
  if (!path) return '';
  if (path.length <= maxLength) return path;
  return '...' + path.slice(-(maxLength - 3));
}

// Get display name for the session (tool name or full command with args)
export function getSessionDisplayName(session: Session): string {
  // If we have a detected tool, show just the tool name
  if (session.tool) {
    return session.tool;
  }
  // Otherwise show full command with arguments
  return getFullCommand(session);
}

// Get full command string with arguments
export function getFullCommand(session: Session): string {
  if (session.args && session.args.length > 0) {
    return `${session.command} ${session.args.join(' ')}`;
  }
  return session.command;
}
