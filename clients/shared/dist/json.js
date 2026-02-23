export function isJsonRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function parseStringRecordLenient(value) {
  if (!isJsonRecord(value)) {
    return {};
  }

  const parsed = {};
  for (const [key, entry] of Object.entries(value)) {
    if (typeof entry === "string") {
      parsed[key] = entry;
    }
  }
  return parsed;
}

export function parseStringRecordStrict(value, fieldName = "value") {
  if (!isJsonRecord(value)) {
    throw new Error(`${fieldName} must be an object`);
  }

  const parsed = {};
  for (const [key, entry] of Object.entries(value)) {
    if (typeof entry !== "string") {
      throw new Error(`${fieldName}.${key} must be a string`);
    }
    parsed[key] = entry;
  }
  return parsed;
}
