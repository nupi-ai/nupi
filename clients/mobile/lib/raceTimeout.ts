/**
 * Race a promise against a timeout with unhandled-rejection safety.
 * Attaches .catch(() => {}) to the input promise and cleans up the timer via .finally().
 * @param errorMessage If provided, rejects with Error on timeout. Otherwise resolves silently.
 */
export function raceTimeout<T>(
  promise: Promise<T>,
  ms: number,
  errorMessage: string,
): Promise<T>;
export function raceTimeout<T>(
  promise: Promise<T>,
  ms: number,
): Promise<T | void>;
export async function raceTimeout<T>(
  promise: Promise<T>,
  ms: number,
  errorMessage?: string,
): Promise<T | void> {
  promise.catch(() => {});
  let timer: ReturnType<typeof setTimeout> | null = null;
  const timeout = errorMessage
    ? new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(errorMessage)), ms);
      })
    : new Promise<void>((resolve) => {
        timer = setTimeout(resolve, ms);
      });
  return Promise.race([promise, timeout])
    .finally(() => { if (timer) clearTimeout(timer); });
}
