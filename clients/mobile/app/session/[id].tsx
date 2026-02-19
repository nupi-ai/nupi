import { useLocalSearchParams, router } from "expo-router";
import { useCallback, useMemo, useRef, useState } from "react";
import {
  ActivityIndicator,
  Pressable,
  StyleSheet,
  View as RNView,
} from "react-native";
import { WebView, type WebViewMessageEvent } from "react-native-webview";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { Text } from "@/components/Themed";
import { useColorScheme } from "@/components/useColorScheme";
import Colors from "@/constants/Colors";
import { arrayBufferToBase64 } from "@/lib/base64";
import { getTerminalHtml } from "@/lib/terminal-html";
import {
  useSessionStream,
  type WsEvent,
} from "@/lib/useSessionStream";

export default function SessionTerminalScreen() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const colorScheme = useColorScheme() ?? "dark";
  const insets = useSafeAreaInsets();
  const webViewRef = useRef<WebView>(null);
  const [webViewReady, setWebViewReady] = useState(false);
  const webViewReadyRef = useRef(false);
  const [sessionStopped, setSessionStopped] = useState(false);
  const [exitCode, setExitCode] = useState<string | null>(null);
  // Queue output received before the WebView is ready.
  const pendingOutputRef = useRef<string[]>([]);
  const pendingResizeRef = useRef<{ cols: number; rows: number } | null>(null);

  const sendToWebView = useCallback(
    (msg: Record<string, unknown>) => {
      webViewRef.current?.postMessage(JSON.stringify(msg));
    },
    []
  );

  const flushPending = useCallback(() => {
    // Apply resize BEFORE output so xterm.js renders at correct dimensions.
    if (pendingResizeRef.current) {
      sendToWebView({
        type: "resize_instruction",
        cols: pendingResizeRef.current.cols,
        rows: pendingResizeRef.current.rows,
      });
      pendingResizeRef.current = null;
    }

    for (const base64 of pendingOutputRef.current) {
      sendToWebView({ type: "output", data: base64 });
    }
    pendingOutputRef.current = [];
  }, [sendToWebView]);

  const handleOutput = useCallback(
    (data: ArrayBuffer) => {
      const base64 = arrayBufferToBase64(data);
      if (!webViewReadyRef.current) {
        pendingOutputRef.current.push(base64);
        return;
      }
      sendToWebView({ type: "output", data: base64 });
    },
    [sendToWebView]
  );

  const handleEvent = useCallback(
    (event: WsEvent) => {
      if (event.event_type === "resize_instruction" && event.data) {
        const cols = parseInt(event.data.cols, 10);
        const rows = parseInt(event.data.rows, 10);
        if (cols > 0 && rows > 0) {
          if (!webViewReadyRef.current) {
            pendingResizeRef.current = { cols, rows };
            return;
          }
          sendToWebView({ type: "resize_instruction", cols, rows });
        }
      } else if (event.event_type === "stopped") {
        setSessionStopped(true);
        setExitCode(event.data?.exit_code ?? null);
      }
    },
    [sendToWebView]
  );

  const { status: streamStatus, error: streamError, send } = useSessionStream(
    id ?? "",
    { onOutput: handleOutput, onEvent: handleEvent }
  );

  const handleWebViewMessage = useCallback(
    (event: WebViewMessageEvent) => {
      try {
        const msg = JSON.parse(event.nativeEvent.data) as {
          type: string;
          data?: string;
          cols?: number;
          rows?: number;
          message?: string;
        };

        switch (msg.type) {
          case "ready":
            webViewReadyRef.current = true;
            setWebViewReady(true);
            flushPending();
            break;
          case "input":
            // Forward terminal keystrokes as binary WS frame.
            if (msg.data) {
              const encoder = new TextEncoder();
              send(encoder.encode(msg.data).buffer as ArrayBuffer);
            }
            break;
          case "resize":
            // Forward resize as JSON text WS frame.
            if (msg.cols && msg.rows) {
              send(
                JSON.stringify({
                  type: "resize",
                  cols: msg.cols,
                  rows: msg.rows,
                })
              );
            }
            break;
          case "error":
            // Log bridge errors for debugging.
            console.warn("[xterm bridge]", msg.message);
            break;
        }
      } catch {
        // Ignore malformed messages.
      }
    },
    [send, flushPending]
  );

  const handleLayout = useCallback(() => {
    if (webViewReady) {
      webViewRef.current?.injectJavaScript("fitAddon.fit(); true;");
    }
  }, [webViewReady]);

  const terminalHtml = useMemo(() => getTerminalHtml(), []);

  const handleGoBack = useCallback(() => {
    if (router.canGoBack()) {
      router.back();
    } else {
      router.replace("/(tabs)/sessions");
    }
  }, []);

  // Loading state: waiting for WebSocket connection.
  if (streamStatus === "connecting") {
    return (
      <RNView
        style={[
          styles.center,
          { backgroundColor: Colors[colorScheme].background },
        ]}
      >
        <ActivityIndicator
          size="large"
          color={Colors[colorScheme].tint}
          accessibilityLabel="Connecting to session"
        />
        <Text style={styles.statusText}>Connecting to session...</Text>
      </RNView>
    );
  }

  // Error state: WebSocket failed.
  if (streamStatus === "error") {
    return (
      <RNView
        style={[
          styles.center,
          { backgroundColor: Colors[colorScheme].background },
        ]}
      >
        <Text style={[styles.errorText, { color: Colors[colorScheme].danger }]}>
          {streamError ?? "Connection failed"}
        </Text>
        <Pressable
          onPress={handleGoBack}
          style={[
            styles.button,
            { backgroundColor: Colors[colorScheme].tint },
          ]}
          accessibilityRole="button"
          accessibilityLabel="Go back to session list"
        >
          <Text style={[styles.buttonText, { color: Colors[colorScheme].background }]}>Go Back</Text>
        </Pressable>
      </RNView>
    );
  }

  return (
    <RNView style={[styles.container, { backgroundColor: Colors[colorScheme].terminalBackground }]}>
      <WebView
        ref={webViewRef}
        source={{ html: terminalHtml }}
        originWhitelist={["*"]}
        javaScriptEnabled={true}
        onMessage={handleWebViewMessage}
        onLayout={handleLayout}
        style={[styles.webview, { backgroundColor: Colors[colorScheme].terminalBackground }]}
        scrollEnabled={false}
        bounces={false}
        overScrollMode="never"
        showsVerticalScrollIndicator={false}
        showsHorizontalScrollIndicator={false}
        allowsInlineMediaPlayback={true}
        mediaPlaybackRequiresUserAction={false}
        accessibilityLabel="Terminal view for session"
      />

      {sessionStopped && (
        <RNView
          style={[
            styles.stoppedBanner,
            {
              backgroundColor: Colors[colorScheme].surface,
              paddingBottom: Math.max(12, insets.bottom),
            },
          ]}
          accessibilityRole="alert"
        >
          <Text
            style={[
              styles.stoppedText,
              { color: Colors[colorScheme].danger },
            ]}
          >
            Session ended{exitCode !== null ? ` (exit code ${exitCode})` : ""}
          </Text>
          <Pressable
            onPress={handleGoBack}
            style={[
              styles.bannerButton,
              { backgroundColor: Colors[colorScheme].tint },
            ]}
            accessibilityRole="button"
            accessibilityLabel="Return to session list"
          >
            <Text style={[styles.buttonText, { color: Colors[colorScheme].background }]}>Back to Sessions</Text>
          </Pressable>
        </RNView>
      )}
    </RNView>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
  },
  webview: {
    flex: 1,
  },
  center: {
    flex: 1,
    justifyContent: "center",
    alignItems: "center",
    padding: 24,
  },
  statusText: {
    marginTop: 12,
    fontSize: 16,
  },
  errorText: {
    fontSize: 16,
    textAlign: "center",
    marginBottom: 20,
  },
  button: {
    paddingHorizontal: 24,
    paddingVertical: 12,
    borderRadius: 8,
  },
  buttonText: {
    fontSize: 16,
    fontWeight: "600",
  },
  stoppedBanner: {
    position: "absolute",
    bottom: 0,
    left: 0,
    right: 0,
    paddingVertical: 12,
    paddingHorizontal: 16,
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-between",
  },
  stoppedText: {
    fontSize: 14,
    fontWeight: "600",
    flexShrink: 1,
  },
  bannerButton: {
    paddingHorizontal: 16,
    paddingVertical: 8,
    borderRadius: 6,
    marginLeft: 12,
  },
});
