import * as styles from './killConfirmDialogStyles';

interface KillConfirmDialogProps {
  sessionName: string;
  onConfirm: () => void;
  onCancel: () => void;
}

export function KillConfirmDialog({ sessionName, onConfirm, onCancel }: KillConfirmDialogProps) {
  return (
    <div style={styles.overlay} onClick={onCancel}>
      <div
        style={styles.dialog}
        onClick={(e) => e.stopPropagation()}
      >
        <h3 style={styles.title}>
          Kill Session?
        </h3>
        <p style={styles.message}>
          Are you sure you want to kill session <strong>"{sessionName}"</strong>?
          <br /><br />
          The session will be stopped but remain visible in the list.
        </p>
        <div style={styles.actionsRow}>
          <button
            onClick={onCancel}
            style={styles.cancelButton}
          >
            Cancel
          </button>
          <button
            onClick={onConfirm}
            style={styles.confirmButton}
          >
            Kill Session
          </button>
        </div>
      </div>
    </div>
  );
}
