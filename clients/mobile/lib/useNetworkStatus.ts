import { useEffect, useState } from "react";
import NetInfo, { type NetInfoState } from "@react-native-community/netinfo";

interface NetworkStatus {
  /** Device has network connectivity (Wi-Fi, cellular, etc.). */
  isConnected: boolean | null;
  /** Device can reach the internet. null = unknown/checking. */
  isInternetReachable: boolean | null;
}

/**
 * Thin wrapper around NetInfo that exposes network connectivity state.
 *
 * IMPORTANT (FR62): isConnected=true with isInternetReachable=false is a
 * VALID state — the device is on LAN and can reach nupid even without
 * internet. Consumers must NOT treat this as "disconnected".
 */
export function useNetworkStatus(): NetworkStatus {
  const [networkStatus, setNetworkStatus] = useState<NetworkStatus>({
    isConnected: null,
    isInternetReachable: null,
  });

  useEffect(() => {
    const unsubscribe = NetInfo.addEventListener((state: NetInfoState) => {
      setNetworkStatus({
        isConnected: state.isConnected,
        isInternetReachable: state.isInternetReachable,
      });
    });

    return () => {
      unsubscribe();
    };
  }, []);

  return networkStatus;
}
