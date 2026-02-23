import { useEffect, useRef } from "react";

/**
 * Creates a resource once per component lifetime and runs cleanup on unmount.
 */
export function useResourceManager<T>(
  factory: () => T,
  cleanup: (resource: T) => void,
): T {
  const resourceRef = useRef<T | null>(null);
  const cleanupRef = useRef(cleanup);
  cleanupRef.current = cleanup;

  if (resourceRef.current === null) {
    resourceRef.current = factory();
  }

  const resource = resourceRef.current;

  useEffect(
    () => () => {
      cleanupRef.current(resource);
    },
    [resource],
  );

  return resource;
}
