import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type Dispatch,
  type MutableRefObject,
  type SetStateAction,
} from "react";

/**
 * State setter that becomes a no-op after unmount (or when shared mountedRef is false).
 */
export function useSafeState<T>(
  initialValue: T | (() => T),
  mountedRef?: MutableRefObject<boolean>
): [T, Dispatch<SetStateAction<T>>, MutableRefObject<boolean>] {
  const localMountedRef = useRef(true);
  const activeMountedRef = mountedRef ?? localMountedRef;

  const [state, setState] = useState(initialValue);

  const setSafeState = useCallback<Dispatch<SetStateAction<T>>>(
    (nextState) => {
      if (!activeMountedRef.current) return;
      setState(nextState);
    },
    [activeMountedRef]
  );

  useEffect(() => {
    if (mountedRef) return;

    activeMountedRef.current = true;
    return () => {
      activeMountedRef.current = false;
    };
  }, [activeMountedRef, mountedRef]);

  return [state, setSafeState, activeMountedRef];
}
