import type { CSSProperties } from 'react';

export const root: CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  height: '100%',
  width: '100%',
};

const headerBase: CSSProperties = {
  padding: '12px 16px',
  borderBottom: '1px solid #333',
  backgroundColor: '#1a1a1a',
};

export const selectedHeader: CSSProperties = {
  ...headerBase,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
};

export const listHeader: CSSProperties = headerBase;

export const headerLeadingSection: CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: '12px',
};

export const backButton: CSSProperties = {
  padding: '6px 12px',
  backgroundColor: '#333',
  border: 'none',
  borderRadius: '4px',
  color: '#fff',
  cursor: 'pointer',
  fontSize: '14px',
};

export const commandRow: CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: '8px',
};

export const commandTitle: CSSProperties = {
  margin: 0,
  fontSize: '16px',
  fontFamily: 'monospace',
};

export const commandArguments: CSSProperties = {
  fontSize: '15px',
  color: '#999',
  fontFamily: 'monospace',
};

export const headerMetadata: CSSProperties = {
  fontSize: '13px',
  color: '#999',
};

export const playerContainer: CSSProperties = {
  flex: 1,
  padding: '16px',
  backgroundColor: '#0d1117',
  overflow: 'hidden',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
};

export const loadingText: CSSProperties = {
  color: '#999',
};

export const listTitle: CSSProperties = {
  margin: 0,
  fontSize: '16px',
};

export const content: CSSProperties = {
  flex: 1,
  overflow: 'auto',
  padding: '16px',
};

const centeredMessageBase: CSSProperties = {
  textAlign: 'center',
  padding: '32px',
};

export const centeredMessage: CSSProperties = {
  ...centeredMessageBase,
  color: '#999',
};

export const centeredErrorMessage: CSSProperties = {
  ...centeredMessageBase,
  color: '#f87171',
};

export const recordingList: CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: '8px',
};

export const recordingCard: CSSProperties = {
  padding: '12px 16px',
  backgroundColor: '#1a1a1a',
  border: '1px solid #333',
  borderRadius: '6px',
  cursor: 'pointer',
  transition: 'all 0.2s',
};

export const recordingCardHover: CSSProperties = {
  backgroundColor: '#252525',
  borderColor: '#444',
};

export const recordingRow: CSSProperties = {
  display: 'flex',
  justifyContent: 'space-between',
  alignItems: 'start',
};

export const recordingMainColumn: CSSProperties = {
  flex: 1,
};

export const recordingHeadingRow: CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: '8px',
  marginBottom: '4px',
};

export const recordingCommand: CSSProperties = {
  fontSize: '14px',
  fontWeight: 500,
  fontFamily: 'monospace',
};

export const recordingArgs: CSSProperties = {
  fontSize: '13px',
  color: '#999',
  fontFamily: 'monospace',
};

export const recordingToolBadge: CSSProperties = {
  fontSize: '11px',
  padding: '2px 6px',
  backgroundColor: '#333',
  borderRadius: '3px',
  color: '#4ade80',
};

export const recordingSessionId: CSSProperties = {
  fontSize: '12px',
  color: '#666',
};

export const recordingMetaColumn: CSSProperties = {
  textAlign: 'right',
};

export const recordingStartTime: CSSProperties = {
  fontSize: '13px',
  color: '#999',
};

export const recordingDuration: CSSProperties = {
  fontSize: '12px',
  color: '#666',
  marginTop: '2px',
};
