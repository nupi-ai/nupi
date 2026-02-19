import { CameraView, useCameraPermissions } from "expo-camera";
import { router } from "expo-router";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  ActivityIndicator,
  Pressable,
  StyleSheet,
  useWindowDimensions,
  View as RNView,
} from "react-native";

import Colors from "@/constants/Colors";
import { useColorScheme } from "@/components/useColorScheme";
import { Text, View } from "@/components/Themed";
import { useConnection } from "@/lib/ConnectionContext";
import { claimPairing, mapPairingError, parseNupiPairUrl } from "@/lib/pairing";

export default function ScanScreen() {
  const colorScheme = useColorScheme() ?? "light";
  const colors = Colors[colorScheme];
  const { connect } = useConnection();
  const { width: windowWidth } = useWindowDimensions();
  const scanSize = windowWidth * 0.7;

  const [permission, requestPermission] = useCameraPermissions();
  const [scanned, setScanned] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const scanningRef = useRef(false);
  const mountedRef = useRef(true);

  useEffect(() => {
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const handleBarcodeScanned = useCallback(
    async (result: { data: string }) => {
      if (scanningRef.current) return;
      scanningRef.current = true;
      setScanned(true);
      setError(null);
      setLoading(true);

      const parsed = parseNupiPairUrl(result.data);
      if (!parsed) {
        if (mountedRef.current) {
          setError("Not a valid Nupi QR code");
          setLoading(false);
        }
        scanningRef.current = false;
        return;
      }

      try {
        const pairingResult = await claimPairing({
          ...parsed,
          clientName: "Mobile",
        });
        try {
          await connect(pairingResult.config, pairingResult.token);
        } catch {
          // Pairing succeeded but verification failed — navigate back
          // so the home screen can show the error and auto-reconnect can retry
          if (mountedRef.current) {
            if (router.canGoBack()) {
              router.back();
            } else {
              router.replace("/");
            }
          }
          return;
        }
        if (mountedRef.current) {
          if (router.canGoBack()) {
            router.back();
          } else {
            router.replace("/");
          }
        }
      } catch (err) {
        if (mountedRef.current) {
          setError(mapPairingError(err));
          setLoading(false);
          scanningRef.current = false;
        }
      }
    },
    [connect]
  );

  const handleRetry = useCallback(() => {
    setScanned(false);
    setError(null);
    setLoading(false);
    scanningRef.current = false;
  }, []);

  // Permission loading
  if (!permission) {
    return <View style={styles.container} />;
  }

  // Permission not granted
  if (!permission.granted) {
    return (
      <View style={styles.permissionContainer}>
        <Text style={styles.permissionText}>
          Camera permission is required to scan QR codes for pairing.
        </Text>
        <Pressable
          style={({ pressed }) => [
            styles.button,
            { backgroundColor: colors.tint, opacity: pressed ? 0.7 : 1 },
          ]}
          onPress={requestPermission}
          accessibilityRole="button"
          accessibilityLabel="Grant Camera Permission"
        >
          <Text style={styles.buttonText}>Grant Camera Permission</Text>
        </Pressable>
      </View>
    );
  }

  return (
    <RNView style={styles.cameraContainer}>
      <CameraView
        style={StyleSheet.absoluteFillObject}
        facing="back"
        barcodeScannerSettings={{ barcodeTypes: ["qr"] }}
        onBarcodeScanned={scanned ? undefined : handleBarcodeScanned}
      />

      {/* Semi-transparent overlay with scan area cutout */}
      <RNView style={styles.overlay}>
        <RNView style={styles.overlayDark} />
        <RNView style={[styles.overlayMiddle, { height: scanSize }]}>
          <RNView style={styles.overlayDark} />
          <RNView style={{ width: scanSize, height: scanSize }}>
            <RNView style={[styles.corner, styles.cornerTL]} />
            <RNView style={[styles.corner, styles.cornerTR]} />
            <RNView style={[styles.corner, styles.cornerBL]} />
            <RNView style={[styles.corner, styles.cornerBR]} />
          </RNView>
          <RNView style={styles.overlayDark} />
        </RNView>
        <RNView style={styles.overlayBottom}>
          {loading ? (
            <ActivityIndicator size="large" color="#fff" accessibilityLabel="Pairing in progress" />
          ) : error ? (
            <>
              <Text style={[styles.errorText, { color: colors.danger }]}>
                {error}
              </Text>
              <Pressable
                style={({ pressed }) => [
                  styles.button,
                  { backgroundColor: colors.tint, opacity: pressed ? 0.7 : 1 },
                ]}
                onPress={handleRetry}
                accessibilityRole="button"
                accessibilityLabel="Try Again"
              >
                <Text style={styles.buttonText}>Try Again</Text>
              </Pressable>
            </>
          ) : (
            <Text style={styles.instructionText}>
              Scan the QR code shown on your desktop
            </Text>
          )}
        </RNView>
      </RNView>
    </RNView>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
  },
  permissionContainer: {
    flex: 1,
    alignItems: "center",
    justifyContent: "center",
    padding: 20,
  },
  cameraContainer: {
    flex: 1,
    backgroundColor: "#000",
  },
  overlay: {
    ...StyleSheet.absoluteFillObject,
  },
  overlayDark: {
    flex: 1,
    backgroundColor: "rgba(0, 0, 0, 0.6)",
  },
  overlayMiddle: {
    flexDirection: "row",
  },
  corner: {
    position: "absolute",
    width: 24,
    height: 24,
    borderColor: "#fff",
  },
  cornerTL: {
    top: 0,
    left: 0,
    borderTopWidth: 3,
    borderLeftWidth: 3,
  },
  cornerTR: {
    top: 0,
    right: 0,
    borderTopWidth: 3,
    borderRightWidth: 3,
  },
  cornerBL: {
    bottom: 0,
    left: 0,
    borderBottomWidth: 3,
    borderLeftWidth: 3,
  },
  cornerBR: {
    bottom: 0,
    right: 0,
    borderBottomWidth: 3,
    borderRightWidth: 3,
  },
  overlayBottom: {
    flex: 1,
    backgroundColor: "rgba(0, 0, 0, 0.6)",
    alignItems: "center",
    justifyContent: "center",
    paddingHorizontal: 20,
  },
  instructionText: {
    color: "#fff",
    fontSize: 16,
    textAlign: "center",
  },
  errorText: {
    fontSize: 16,
    textAlign: "center",
    marginBottom: 16,
  },
  permissionText: {
    fontSize: 16,
    textAlign: "center",
    marginBottom: 20,
    paddingHorizontal: 20,
  },
  button: {
    paddingHorizontal: 24,
    paddingVertical: 14,
    borderRadius: 12,
  },
  buttonText: {
    color: "#fff",
    fontSize: 16,
    fontWeight: "600",
  },
});
