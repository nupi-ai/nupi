import type { CSSProperties } from 'react';
import { theme } from '../designTokens';

const borderDefault = `1px solid ${theme.border.default}`;

const buttonBase: CSSProperties = {
  padding: '8px 16px',
  color: theme.text.primary,
  border: 'none',
  borderRadius: '4px',
  cursor: 'pointer',
  fontSize: '14px',
};

export const overlay: CSSProperties = {
  position: 'fixed',
  top: 0,
  left: 0,
  right: 0,
  bottom: 0,
  backgroundColor: theme.bg.overlayDialog,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  zIndex: 1000,
};

export const dialog: CSSProperties = {
  backgroundColor: theme.bg.nav,
  border: borderDefault,
  borderRadius: '8px',
  padding: '24px',
  minWidth: '400px',
  maxWidth: '500px',
};

export const title: CSSProperties = {
  margin: '0 0 16px 0',
  color: theme.text.primary,
  fontSize: '18px',
};

export const message: CSSProperties = {
  margin: '0 0 24px 0',
  color: theme.text.light,
  lineHeight: '1.5',
};

export const actionsRow: CSSProperties = {
  display: 'flex',
  gap: '12px',
  justifyContent: 'flex-end',
};

export const cancelButton: CSSProperties = {
  ...buttonBase,
  backgroundColor: theme.bg.neutral,
};

export const confirmButton: CSSProperties = {
  ...buttonBase,
  backgroundColor: theme.bg.dangerButton,
};
