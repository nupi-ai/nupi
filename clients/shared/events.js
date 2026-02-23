export function createEventParser(validTypes) {
  const typeSet = new Set(validTypes);
  return (value) => {
    if (typeof value !== "string") {
      return null;
    }
    return typeSet.has(value) ? value : null;
  };
}

export function parseSessionId(value) {
  if (typeof value !== "string" || value.length === 0) {
    return null;
  }
  return value;
}
