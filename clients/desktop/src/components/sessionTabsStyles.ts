import type { CSSProperties } from 'react';

export const tabsContainer: CSSProperties = {
  display: 'flex',
  borderBottom: '1px solid #333',
  backgroundColor: '#2d2d2d',
  minHeight: '48px',
  overflowX: 'auto',
  overflowY: 'hidden',
  padding: '8px 8px 0 8px',
  gap: '4px',
  alignItems: 'flex-end',
};

export const emptyStateText: CSSProperties = {
  padding: '0 16px',
  color: '#666',
  fontSize: '13px',
  display: 'flex',
  alignItems: 'center',
  height: '100%',
};

const tabBase: CSSProperties = {
  padding: '8px 16px',
  boxSizing: 'border-box',
  borderLeft: '1px solid #333',
  borderRight: '1px solid #333',
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
    borderTop: active ? '3px solid #4ade80' : '3px solid #ef4444',
    borderBottom: isSelected ? '1px solid #262626' : '1px solid #333',
    backgroundColor: isSelected ? '#262626' : '#1a1a1a',
    color: isSelected && active ? '#fff' : '#999',
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
  background: 'transparent',
  border: 'none',
  color: '#999',
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
  background: 'transparent',
  color: '#999',
};

const killButtonHoverState: ActionButtonState = {
  background: 'rgba(239, 68, 68, 0.2)',
  color: '#ef4444',
};

const playButtonHoverState: ActionButtonState = {
  background: 'rgba(74, 222, 128, 0.2)',
  color: '#4ade80',
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
