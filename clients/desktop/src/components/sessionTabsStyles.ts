import type { CSSProperties } from 'react';
import { theme } from '../designTokens';

const borderDefault = `1px solid ${theme.border.default}`;

export const tabsContainer: CSSProperties = {
  display: 'flex',
  borderBottom: borderDefault,
  backgroundColor: theme.bg.tabs,
  minHeight: '48px',
  overflowX: 'auto',
  overflowY: 'hidden',
  padding: '8px 8px 0 8px',
  gap: '4px',
  alignItems: 'flex-end',
};

export const emptyStateText: CSSProperties = {
  padding: '0 16px',
  color: theme.text.subdued,
  fontSize: '13px',
  display: 'flex',
  alignItems: 'center',
  height: '100%',
};

const tabBase: CSSProperties = {
  padding: '8px 16px',
  boxSizing: 'border-box',
  borderLeft: borderDefault,
  borderRight: borderDefault,
  cursor: 'pointer',
  fontFamily: 'system-ui, -apple-system, sans-serif',
  minWidth: '120px',
  maxWidth: '250px',
  height: 'fit-content',
  position: 'relative',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'flex-start',
  textAlign: 'left',
  borderRadius: '6px 6px 0 0',
  marginBottom: '-1px',
  transition: 'all 0.2s ease',
};

export function sessionTab(active: boolean, isSelected: boolean): CSSProperties {
  return {
    ...tabBase,
    borderTop: active
      ? `3px solid ${theme.border.active}`
      : `3px solid ${theme.border.inactive}`,
    borderBottom: isSelected
      ? `1px solid ${theme.border.selected}`
      : borderDefault,
    backgroundColor: isSelected ? theme.bg.tabSelected : theme.bg.nav,
    color: isSelected && active ? theme.text.primary : theme.text.muted,
    opacity: active ? 1 : 0.6,
  };
}

export const tabHeaderRow: CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  fontSize: '13px',
  fontWeight: 500,
  marginBottom: '2px',
  width: '100%',
  gap: '6px',
};

export const tabLabel: CSSProperties = {
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  flex: 1,
};

export const tabActionButton: CSSProperties = {
  background: theme.bg.transparent,
  border: 'none',
  color: theme.text.muted,
  cursor: 'pointer',
  padding: '2px 4px',
  fontSize: '14px',
  lineHeight: '14px',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  borderRadius: '3px',
  transition: 'all 0.15s ease',
};

interface ActionButtonState {
  background: string;
  color: string;
}

const actionButtonDefaultState: ActionButtonState = {
  background: theme.bg.transparent,
  color: theme.text.muted,
};

const killButtonHoverState: ActionButtonState = {
  background: theme.bg.dangerHover,
  color: theme.text.dangerStrong,
};

const playButtonHoverState: ActionButtonState = {
  background: theme.bg.successHover,
  color: theme.text.success,
};

function applyButtonState(button: HTMLElement, state: ActionButtonState): void {
  button.style.background = state.background;
  button.style.color = state.color;
}

export function highlightKillButton(button: HTMLElement): void {
  applyButtonState(button, killButtonHoverState);
}

export function highlightPlayButton(button: HTMLElement): void {
  applyButtonState(button, playButtonHoverState);
}

export function resetTabActionButton(button: HTMLElement): void {
  applyButtonState(button, actionButtonDefaultState);
}

export const workingDirectoryText: CSSProperties = {
  fontSize: '11px',
  opacity: 0.7,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  width: '100%',
};
