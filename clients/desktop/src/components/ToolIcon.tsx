import { useMemo } from 'react';

interface ToolIconProps {
  iconData?: string; // Base64 encoded icon data
  toolName: string;
  size?: number;
}

// Dangerous element/attribute patterns in SVG content.
const DANGEROUS_SVG_PATTERN = /<script[\s>]|on\w+\s*=|javascript:|<foreignObject[\s>]|<iframe[\s>]|<embed[\s>]|<object[\s>]|<use[\s>]|<image[\s>]|xlink:href/i;

// Reject SVGs containing XML/HTML entity references (&#...; or &name;).
// Legitimate tool icons don't need entity encoding, and entities can be used
// to bypass the keyword-based pattern check above.
const ENTITY_ENCODED_PATTERN = /&#\w*;|&lt;/i;

function sanitizeIconData(raw: string): string | null {
  let decoded: string;
  try {
    decoded = atob(raw);
  } catch {
    return null;
  }

  const trimmed = decoded.trimStart();
  const looksLikeSvg = trimmed.startsWith('<svg') || trimmed.startsWith('<?xml');
  if (!looksLikeSvg) {
    return null;
  }

  if (DANGEROUS_SVG_PATTERN.test(decoded)) {
    return null;
  }

  if (ENTITY_ENCODED_PATTERN.test(decoded)) {
    return null;
  }

  return raw;
}

function DefaultIcon({ size }: { size: number }) {
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

export function ToolIcon({ iconData, toolName, size = 16 }: ToolIconProps) {
  const safeData = useMemo(
    () => iconData ? sanitizeIconData(iconData) : null,
    [iconData],
  );

  if (!safeData) {
    return <DefaultIcon size={size} />;
  }

  return (
    <img
      src={`data:image/svg+xml;base64,${safeData}`}
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
