import { useLocalSearchParams, router, useNavigation } from "expo-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ActivityIndicator,
  Keyboard,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  StyleSheet,
  View as RNView,
} from "react-native";
import { WebView, type WebViewMessageEvent } from "react-native-webview";
import { useHeaderHeight } from "@react-navigation/elements";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { SpecialKeysToolbar } from "@/components/SpecialKeysToolbar";
import { Text } from "@/components/Themed";
import { useColorScheme } from "@/components/useColorScheme";
import Colors from "@/constants/Colors";
import { arrayBufferToBase64 } from "@/lib/base64";
import { getTerminalHtml } from "@/lib/terminal-html";
import {
  useSessionStream,
  type WsEvent,
} from "@/lib/useSessionStream";

/** Reusable encoder — TextEncoder is stateless, no need to allocate per-call. */
const textEncoder = new TextEncoder();

/** Encode a string to an ArrayBuffer for binary WebSocket transmission. */
function encodeToBuffer(data: string): ArrayBuffer {
  return textEncoder.encode(data).buffer as ArrayBuffer;
}

export default function SessionTerminalScreen() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const colorScheme = useColorScheme() ?? "dark";
  const headerHeight = useHeaderHeight();
  const insets = useSafeAreaInsets();
  const webViewRef = useRef<WebView>(null);
  const webViewReadyRef = useRef(false);
  const navigation = useNavigation();
  const [sessionStopped, setSessionStopped] = useState(false);
  const sessionStoppedRef = useRef(false);
  const [exitCode, setExitCode] = useState<string | null>(null);
  const [keyboardVisible, setKeyboardVisible] = useState(false);
  const keyboardVisibleRef = useRef(false);
  const [ctrlActive, setCtrlActive] = useState(false);
  const ctrlActiveRef = useRef(false);
  const [recoveryCrashCount, setRecoveryCrashCount] = useState(0);
  // Queue output received before the WebView is ready.
  const pendingOutputRef = useRef<string[]>([]);
  const pendingResizeRef = useRef<{ cols: number; rows: number } | null>(null);
  const fitDebounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

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
        sessionStoppedRef.current = true;
        setSessionStopped(true);
        setExitCode(event.data?.exit_code ?? null);
        sendToWebView({ type: "blur_keyboard" });
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
            flushPending();
            break;
          case "tap":
            // User tapped terminal area — focus to show keyboard.
            if (!sessionStoppedRef.current) {
              sendToWebView({ type: "focus_keyboard" });
            }
            break;
          case "input":
            // Forward terminal keystrokes as binary WS frame.
            if (msg.data && !sessionStoppedRef.current) {
              let inputData = msg.data;
              // Apply Ctrl modifier from toolbar toggle.
              if (ctrlActiveRef.current && inputData.length === 1) {
                const upper = inputData.toUpperCase();
                if (upper >= "A" && upper <= "Z") {
                  inputData = String.fromCharCode(upper.charCodeAt(0) - 64);
                  ctrlActiveRef.current = false;
                  setCtrlActive(false);
                }
              }
              send(encodeToBuffer(inputData));
            }
            break;
          case "resize": {
            if (sessionStoppedRef.current) break;
            // Forward resize as JSON text WS frame.
            const resizeCols = msg.cols ?? 0;
            const resizeRows = msg.rows ?? 0;
            if (resizeCols > 0 && resizeRows > 0) {
              send(
                JSON.stringify({
                  type: "resize",
                  cols: resizeCols,
                  rows: resizeRows,
                })
              );
            }
            break;
          }
          case "error":
            // Log bridge errors for debugging.
            console.warn("[xterm bridge]", msg.message);
            break;
        }
      } catch (e) {
        if (!(e instanceof SyntaxError)) {
          console.warn("[handleWebViewMessage]", e);
        }
      }
    },
    [send, flushPending, sendToWebView]
  );

  const handleLayout = useCallback(() => {
    if (webViewReadyRef.current) {
      if (fitDebounceRef.current) {
        clearTimeout(fitDebounceRef.current);
      }
      fitDebounceRef.current = setTimeout(() => {
        webViewRef.current?.injectJavaScript("fitAddon.fit(); true;");
        fitDebounceRef.current = null;
      }, 150);
    }
  }, []);

  const terminalHtml = useMemo(() => getTerminalHtml(), []);

  const handleGoBack = useCallback(() => {
    if (router.canGoBack()) {
      router.back();
    } else {
      router.replace("/(tabs)/sessions");
    }
  }, []);

  const toggleKeyboard = useCallback(() => {
    if (keyboardVisibleRef.current) {
      sendToWebView({ type: "blur_keyboard" });
    } else {
      sendToWebView({ type: "focus_keyboard" });
    }
  }, [sendToWebView]);

  const handleSendInput = useCallback(
    (data: string) => {
      if (sessionStoppedRef.current) return;
      send(encodeToBuffer(data));
    },
    [send]
  );

  const handleCtrlToggle = useCallback(() => {
    const next = !ctrlActiveRef.current;
    ctrlActiveRef.current = next;
    setCtrlActive(next);
  }, []);

  const handleWebViewCrash = useCallback(() => {
    console.warn("[WebView] process terminated — reloading");
    webViewReadyRef.current = false;
    pendingOutputRef.current = [];
    pendingResizeRef.current = null;
    setRecoveryCrashCount(c => c + 1);
    webViewRef.current?.reload();
  }, []);

  // Auto-dismiss recovery notice after 4 seconds.
  useEffect(() => {
    if (recoveryCrashCount > 0) {
      const timer = setTimeout(() => setRecoveryCrashCount(0), 4000);
      return () => clearTimeout(timer);
    }
  }, [recoveryCrashCount]);

  // Keyboard visibility tracking
  useEffect(() => {
    const showEvent = Platform.OS === "ios" ? "keyboardWillShow" : "keyboardDidShow";
    const hideEvent = Platform.OS === "ios" ? "keyboardWillHide" : "keyboardDidHide";
    const showSub = Keyboard.addListener(showEvent, () => {
      keyboardVisibleRef.current = true;
      setKeyboardVisible(true);
    });
    const hideSub = Keyboard.addListener(hideEvent, () => {
      keyboardVisibleRef.current = false;
      setKeyboardVisible(false);
      // Reset Ctrl modifier so it doesn't silently persist across keyboard sessions.
      ctrlActiveRef.current = false;
      setCtrlActive(false);
    });
    return () => {
      showSub.remove();
      hideSub.remove();
      if (fitDebounceRef.current) {
        clearTimeout(fitDebounceRef.current);
      }
    };
  }, []);

  // Header keyboard toggle button — only shown when terminal is active.
  // Note: Terminal refit on keyboard show/hide is handled by onLayout on the WebView,
  // which fires when KeyboardAvoidingView changes the WebView's layout dimensions.
  useEffect(() => {
    const showToggle = streamStatus === "connected" && !sessionStopped;
    navigation.setOptions({
      headerRight: showToggle
        ? () => (
            <Pressable
              onPress={toggleKeyboard}
              style={styles.headerButton}
              accessibilityRole="button"
              accessibilityLabel="Toggle keyboard"
              testID="keyboard-toggle"
              accessibilityState={{ expanded: keyboardVisible }}
            >
              <Text style={[styles.headerButtonText, { color: Colors[colorScheme].tint }]}>
                {keyboardVisible ? "⌨\uFE0F\u2713" : "⌨\uFE0F"}
              </Text>
            </Pressable>
          )
        : undefined,
    });
  }, [navigation, toggleKeyboard, keyboardVisible, colorScheme, streamStatus, sessionStopped]);

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
          testID="go-back-button"
        >
          <Text style={[styles.buttonText, { color: Colors[colorScheme].background }]}>Go Back</Text>
        </Pressable>
      </RNView>
    );
  }

  return (
    <KeyboardAvoidingView
      style={[styles.container, { backgroundColor: Colors[colorScheme].terminalBackground }]}
      behavior={Platform.OS === "ios" ? "padding" : undefined}
      keyboardVerticalOffset={Platform.OS === "ios" ? headerHeight : 0}
    >
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
        keyboardDisplayRequiresUserAction={false}
        hideKeyboardAccessoryView={true}
        accessibilityLabel="Terminal view for session"
        onContentProcessDidTerminate={handleWebViewCrash}
        onRenderProcessGone={handleWebViewCrash}
      />

      {keyboardVisible && !sessionStopped && (
        <SpecialKeysToolbar
          onSendInput={handleSendInput}
          ctrlActive={ctrlActive}
          onCtrlToggle={handleCtrlToggle}
        />
      )}

      {recoveryCrashCount > 0 && (
        <RNView
          style={[
            styles.recoveryBanner,
            { backgroundColor: Colors[colorScheme].warning },
          ]}
          accessibilityRole="alert"
        >
          <Text style={[styles.recoveryText, { color: Colors[colorScheme].onWarning }]}>
            Terminal view recovered — history unavailable
          </Text>
        </RNView>
      )}

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
            testID="back-to-sessions-button"
          >
            <Text style={[styles.buttonText, { color: Colors[colorScheme].background }]}>Back to Sessions</Text>
          </Pressable>
        </RNView>
      )}
    </KeyboardAvoidingView>
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
  recoveryBanner: {
    position: "absolute",
    top: 0,
    left: 0,
    right: 0,
    paddingVertical: 8,
    paddingHorizontal: 16,
    alignItems: "center",
    zIndex: 1,
  },
  recoveryText: {
    fontSize: 13,
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
    zIndex: 1,
  },
  stoppedText: {
    fontSize: 14,
    fontWeight: "600",
    flexShrink: 1,
  },
  bannerButton: {
    paddingHorizontal: 16,
    paddingVertical: 12,
    borderRadius: 6,
    marginLeft: 12,
  },
  headerButton: {
    paddingHorizontal: 12,
    paddingVertical: 12,
  },
  headerButtonText: {
    fontSize: 20,
  },
});
