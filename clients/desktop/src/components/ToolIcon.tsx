interface ToolIconProps {
  iconData?: string; // Base64 encoded icon data
  toolName: string;
  size?: number;
}

export function ToolIcon({ iconData, toolName, size = 16 }: ToolIconProps) {

  if (!iconData) {
    // Default icon for unknown tools
    return (
      <svg
        width={size}
        height={size}
        viewBox="0 0 16 16"
        fill="none"
        style={{ marginRight: '4px' }}
      >
        <rect x="2" y="2" width="12" height="12" rx="2" stroke="currentColor" strokeWidth="1.5"/>
        <circle cx="8" cy="8" r="2" fill="currentColor"/>
      </svg>
    );
  }

  // Use data URL with base64 icon
  const dataUrl = `data:image/svg+xml;base64,${iconData}`;

  return (
    <img
      src={dataUrl}
      alt={toolName}
      width={size}
      height={size}
      style={{
        marginRight: '4px',
        objectFit: 'contain',
      }}
    />
  );
}