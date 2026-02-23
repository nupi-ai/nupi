export type JsonRecord = Record<string, unknown>;

export declare function isJsonRecord(value: unknown): value is JsonRecord;
export declare function parseStringRecordLenient(value: unknown): Record<string, string>;
export declare function parseStringRecordStrict(
  value: unknown,
  fieldName?: string
): Record<string, string>;
