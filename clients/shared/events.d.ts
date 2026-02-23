export declare function createEventParser<const T extends readonly string[]>(
  validTypes: T
): (value: unknown) => T[number] | null;

export declare function parseSessionId(value: unknown): string | null;
