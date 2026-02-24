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

import { ConversationPanel } from "@/components/ConversationPanel";
import { ModelDownloadSheet } from "@/components/ModelDownloadSheet";
import { Button } from "@/components/Button";
import { SpecialKeysToolbar } from "@/components/SpecialKeysToolbar";
import { Text } from "@/components/Themed";
import { TranscriptionBubble } from "@/components/TranscriptionBubble";
import { useColorScheme } from "@/components/useColorScheme";
import { VoiceButton } from "@/components/VoiceButton";
import Colors from "@/constants/designTokens";
import { arrayBufferToBase64 } from "@/lib/base64";
import { useConnection } from "@/lib/ConnectionContext";
import { getTerminalHtml } from "@/lib/terminal-html";
import { useConversation } from "@/lib/useConversation";
import {
  useSessionStream,
  type WsEvent,
} from "@/lib/useSessionStream";
import { useVoice } from "@/lib/VoiceContext";
import { useNotifications } from "@/lib/NotificationContext";
import { wasNotificationPermissionPrompted, markNotificationPermissionPrompted } from "@/lib/storage";
import { raceTimeout } from "@/lib/raceTimeout";

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
  const [showDownloadSheet, setShowDownloadSheet] = useState(false);
  const {
    modelStatus,
    recordingStatus,
    confirmedText,
    pendingText,
    voiceError,
    downloadProgress,
    downloadStage,
    initModel,
    startRecording: voiceStartRecording,
    stopRecording: voiceStopRecording,
    clearTranscription,
    clearVoiceError,
  } = useVoice();
  const modelStatusRef = useRef(modelStatus);
  modelStatusRef.current = modelStatus;
  const { client } = useConnection();
  const { permissionGranted, requestPermission } = useNotifications();
  // H2 fix (Review 13): Use refs for notification values inside the voice
  // command effect to avoid re-running the effect (and potentially double-sending
  // commands) when permission state changes.
  const permissionGrantedRef = useRef(permissionGranted);
  permissionGrantedRef.current = permissionGranted;
  const requestPermissionRef = useRef(requestPermission);
  requestPermissionRef.current = requestPermission;
  const [voiceCommandStatus, setVoiceCommandStatus] = useState<
    "idle" | "sending" | "thinking" | "error"
  >("idle");
  const voiceCommandStatusRef = useRef(voiceCommandStatus);
  voiceCommandStatusRef.current = voiceCommandStatus;
  const isSendingRef = useRef(false);
  const pendingCommandsRef = useRef(0);
  // L2 fix: track error recovery timer so it can be cleared on unmount.
  const errorRecoveryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const screenMountedRef = useRef(true);
  // NOTE: useConversation is called after useSessionStream (below) so that
  // streamStatus is available for the isConnected parameter.
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

  const handleReconnected = useCallback(() => {
    // Clear terminal before history replay to prevent duplicate output
    sendToWebView({ type: "clear" });
  }, [sendToWebView]);

  const {
    status: streamStatus,
    error: streamError,
    reconnectAttempts: wsReconnectAttempts,
    send,
    reconnect: wsReconnect,
  } = useSessionStream(
    id ?? "",
    { onOutput: handleOutput, onEvent: handleEvent, onReconnected: handleReconnected }
  );

  // Ref for stable access in callbacks without triggering recreations.
  const streamStatusRef = useRef(streamStatus);
  streamStatusRef.current = streamStatus;

  // M2 fix: pass isConnected so useConversation pauses polling when disconnected.
  const {
    turns,
    isPolling,
    error: conversationError,
    startPolling,
    stopPolling,
    refresh,
    addOptimistic,
    removeLastOptimistic,
  } = useConversation(id ?? "", client, streamStatus === "connected");
  const prevTurnsLenRef = useRef(turns.length);

  // H1 fix: refresh conversation after WebSocket reconnection so that AI
  // responses that arrived during the outage become visible.
  const prevStreamStatusRef = useRef(streamStatus);
  useEffect(() => {
    const prev = prevStreamStatusRef.current;
    prevStreamStatusRef.current = streamStatus;
    if (streamStatus === "connected" && prev === "reconnecting") {
      refresh();
    }
  }, [streamStatus, refresh]);

  const handleRetryStream = useCallback(() => {
    wsReconnect();
  }, [wsReconnect]);

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
            if (!sessionStoppedRef.current && streamStatusRef.current === "connected") {
              sendToWebView({ type: "focus_keyboard" });
            }
            break;
          case "input":
            // Forward terminal keystrokes as binary WS frame.
            if (msg.data && !sessionStoppedRef.current && streamStatusRef.current === "connected") {
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

  const handleVoicePress = useCallback(() => {
    const status = modelStatusRef.current;
    if (status === "not_downloaded" || status === "downloading" || status === "initializing") {
      setShowDownloadSheet(true);
    }
  }, []);

  const handleDownloadSheetCancel = useCallback(() => {
    setShowDownloadSheet(false);
  }, []);

  const handleWebViewCrash = useCallback(() => {
    console.warn("[WebView] process terminated — reloading");
    webViewReadyRef.current = false;
    pendingOutputRef.current = [];
    pendingResizeRef.current = null;
    setRecoveryCrashCount(c => c + 1);
    webViewRef.current?.reload();
  }, []);

  // Stop recording, polling, and clear transcript when leaving the session screen.
  // VoiceProvider is app-scoped, so without this the native audio stream
  // would keep recording in the background and transcript text would
  // carry over to a different session screen.
  useEffect(() => {
    return () => {
      screenMountedRef.current = false;
      voiceStopRecording();
      clearTranscription();
      clearVoiceError();
      stopPolling();
      pendingCommandsRef.current = 0;
      // L2 fix: clear error recovery timer on unmount.
      if (errorRecoveryTimerRef.current) {
        clearTimeout(errorRecoveryTimerRef.current);
        errorRecoveryTimerRef.current = null;
      }
    };
  }, [voiceStopRecording, clearTranscription, clearVoiceError, stopPolling]);

  // Auto-send voice command when recording finishes with confirmed text.
  useEffect(() => {
    if (recordingStatus !== "result" || !confirmedText || isSendingRef.current) return;
    if (!client) return;

    isSendingRef.current = true;
    setVoiceCommandStatus("sending");

    // Request notification permission on first voice command (Task 5.5).
    // Fire-and-forget: don't block voice command send on permission result.
    // H2 fix (Review 13): Read from refs to avoid including in effect deps.
    // M2 fix (Review 14): Check wasNotificationPermissionPrompted() BEFORE
    // checking permissionGranted so we don't re-prompt on every command after
    // the user denied permissions. Once prompted, never ask again (OS blocks
    // it anyway after first denial).
    if (!permissionGrantedRef.current) {
      wasNotificationPermissionPrompted().then((prompted) => {
        if (!prompted) {
          requestPermissionRef.current().then(() =>
            markNotificationPermissionPrompted()
          ).catch((err) => {
            // M5 fix (Review 14): log instead of silently swallowing.
            console.warn("[Notifications] requestPermission failed:", err);
          });
        }
      }).catch((err) => {
        // M5 fix (Review 14): log instead of silently swallowing.
        console.warn("[Notifications] wasNotificationPermissionPrompted failed:", err);
      });
    }

    const text = confirmedText;
    addOptimistic(text);
    clearTranscription();

    (async () => {
      try {
        // H2 fix: timeout on Connect RPC send (10s per Story 11-1 intelligence).
        const resp = await raceTimeout(
          client.sessions.sendVoiceCommand({
            sessionId: id ?? "",
            text,
            metadata: { client_type: "mobile" },
          }),
          10_000,
          "Voice command timed out",
        );
        if (resp && !resp.accepted) {
          throw new Error(resp.message || "Command rejected by server");
        }
        pendingCommandsRef.current += 1;
        if (voiceCommandStatusRef.current !== "idle") {
          setVoiceCommandStatus("thinking");
        }
        startPolling();
      } catch (e) {
        // H1 fix (Review 10): SendVoiceCommand is fire-and-forget — the server
        // likely received and processed the command even when the HTTP response
        // times out. Start polling so the AI response isn't silently lost.
        const isTimeout = e instanceof Error && e.message.includes("timed out");
        if (isTimeout) {
          console.warn("[voice-command] send timed out, polling for possible response");
          pendingCommandsRef.current += 1;
          setVoiceCommandStatus("thinking");
          startPolling();
        } else {
          // L2 fix (Review 10): log full error object for stack trace debugging.
          console.error("[voice-command] send failed:", e);
          setVoiceCommandStatus("error");
          removeLastOptimistic();
          // M1 fix: Auto-reset from error after 3s so user can retry.
          // L2 fix: store timer ref so it can be cleared on unmount.
          // M1 fix (Review 6): clear any existing recovery timer before setting
          // a new one to prevent orphaned timers on rapid consecutive errors.
          if (errorRecoveryTimerRef.current) {
            clearTimeout(errorRecoveryTimerRef.current);
          }
          errorRecoveryTimerRef.current = setTimeout(() => {
            errorRecoveryTimerRef.current = null;
            if (voiceCommandStatusRef.current === "error") {
              setVoiceCommandStatus("idle");
            }
          }, 3000);
        }
      } finally {
        isSendingRef.current = false;
      }
    })();
  // H2 fix (Review 13): permissionGranted/requestPermission removed from deps
  // (read via refs) to prevent re-running this effect when permission changes.
  }, [recordingStatus, confirmedText, client, id, addOptimistic, removeLastOptimistic, clearTranscription, startPolling]);

  // Detect AI response arrival — decrement pending counter, stop polling when all answered.
  useEffect(() => {
    // Always track turns length so it stays current after refresh/reconnection
    // even when voiceCommandStatus is "idle". Without this, prevTurnsLenRef
    // becomes stale and causes phantom AI detection on the next voice command.
    const prevLen = prevTurnsLenRef.current;
    prevTurnsLenRef.current = turns.length;

    if (voiceCommandStatusRef.current !== "thinking" && voiceCommandStatusRef.current !== "sending") return;
    // Check if new AI turns appeared since last render.
    // Decrement by at most 1 per render cycle to avoid multi-turn AI responses
    // (tool-use, multi-step) from draining the counter too fast.
    if (turns.length > prevLen) {
      const newTurns = turns.slice(prevLen);
      const hasNewAi = newTurns.some(t => t.origin === "ai" && !t.isOptimistic);
      if (hasNewAi) {
        pendingCommandsRef.current = Math.max(0, pendingCommandsRef.current - 1);
        if (pendingCommandsRef.current === 0) {
          setVoiceCommandStatus("idle");
          stopPolling();
        }
      }
    }
  }, [turns, stopPolling]);

  // H1 fix: Reset voiceCommandStatus when polling auto-stops without AI response.
  // useConversation stops polling after 60s; without this, the thinking spinner
  // would persist forever.
  useEffect(() => {
    if (!isPolling && voiceCommandStatusRef.current === "thinking") {
      setVoiceCommandStatus("idle");
      pendingCommandsRef.current = 0;
    }
  }, [isPolling]);

  // Auto-close download sheet when model becomes ready.
  useEffect(() => {
    if (modelStatus === "ready" && showDownloadSheet) {
      setShowDownloadSheet(false);
    }
  }, [modelStatus, showDownloadSheet]);

  // Stop recording, polling, and clear transcription when session ends.
  // The "stopped" event sets sessionStopped which unmounts the voiceRow
  // (hiding VoiceButton), but the WebSocket may still be "connected" so
  // the disconnect effect below won't fire. clearTranscription prevents
  // the orphaned TranscriptionBubble from floating over the "Session ended" banner.
  useEffect(() => {
    if (sessionStopped) {
      voiceStopRecording();
      clearTranscription();
      clearVoiceError();
      stopPolling();
      setVoiceCommandStatus("idle");
      pendingCommandsRef.current = 0;
    }
  }, [sessionStopped, voiceStopRecording, clearTranscription, clearVoiceError, stopPolling]);

  // Dismiss keyboard, stop recording and polling when stream disconnects.
  // Without this, a disconnect while recording leaves the native audio stream
  // capturing in the background with no visible VoiceButton to stop it
  // (voiceRow unmounts when streamStatus !== "connected").
  // Also reset WebView ready state when WebView will be unmounted (error/connecting
  // early returns) so that output arriving before the new WebView initializes gets
  // queued in pendingOutputRef instead of being sent to a stale/loading WebView.
  useEffect(() => {
    if (streamStatus === "reconnecting" || streamStatus === "error") {
      Keyboard.dismiss();
      voiceStopRecording();
      // Clear transcription and errors — without this, TranscriptionBubble
      // persists over the reconnect overlay (it has no streamStatus guard)
      // and becomes non-interactive due to overlay's pointerEvents="box-only".
      clearTranscription();
      clearVoiceError();
      stopPolling();
      setVoiceCommandStatus("idle");
      isSendingRef.current = false; // M2 fix: unblock sends after reconnect
      pendingCommandsRef.current = 0;
    }
    if (streamStatus === "error" || streamStatus === "connecting") {
      webViewReadyRef.current = false;
    }
  }, [streamStatus, voiceStopRecording, clearTranscription, clearVoiceError, stopPolling]);

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
                {keyboardVisible ? "\u2328\uFE0F\u2713" : "\u2328\uFE0F"}
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
        <Button
          onPress={handleRetryStream}
          style={styles.button}
          variant="primary"
          color={Colors[colorScheme].tint}
          accessibilityLabel="Retry connection"
          accessibilityHint="Attempts to reconnect the WebSocket stream"
          testID="retry-stream-button"
        >
          <Text style={[styles.buttonText, { color: Colors[colorScheme].background }]}>Retry</Text>
        </Button>
        <Button
          onPress={() => router.push("/scan")}
          style={styles.goBackButton}
          variant="outline"
          color={Colors[colorScheme].tint}
          accessibilityLabel="Re-pair by scanning QR code"
          accessibilityHint="Opens camera to scan a new QR code for pairing with nupid"
          testID="re-pair-stream-button"
        >
          <Text style={[styles.buttonText, { color: Colors[colorScheme].tint }]}>Re-pair (scan QR)</Text>
        </Button>
        <Button
          onPress={() => router.push("/(tabs)/settings")}
          style={styles.goBackButton}
          variant="outline"
          color={Colors[colorScheme].tint}
          accessibilityLabel="Go to settings"
          accessibilityHint="View connection details and manage pairing"
          testID="session-go-to-settings-button"
        >
          <Text style={[styles.buttonText, { color: Colors[colorScheme].tint }]}>Go to Settings</Text>
        </Button>
        <Button
          onPress={handleGoBack}
          style={styles.goBackButton}
          variant="outline"
          color={Colors[colorScheme].tint}
          accessibilityLabel="Go back to session list"
          accessibilityHint="Returns to the session list screen"
          testID="go-back-button"
        >
          <Text style={[styles.buttonText, { color: Colors[colorScheme].tint }]}>Go Back</Text>
        </Button>
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

      {(turns.length > 0 || voiceCommandStatus !== "idle") && streamStatus === "connected" && (
        <ConversationPanel
          turns={turns}
          isThinking={voiceCommandStatus === "thinking"}
          sendError={voiceCommandStatus === "error"}
          pollError={conversationError}
        />
      )}

      {(confirmedText || pendingText) && streamStatus === "connected" && (recordingStatus === "recording" || recordingStatus === "result") && (
        <TranscriptionBubble
          confirmedText={confirmedText}
          pendingText={pendingText}
          isLive={recordingStatus === "recording"}
          onDismiss={clearTranscription}
        />
      )}

      {!sessionStopped && streamStatus === "connected" && (
        <RNView style={styles.voiceRow}>
          {voiceError && !showDownloadSheet && (
            <Text
              style={[styles.voiceErrorText, { color: Colors[colorScheme].danger }]}
              numberOfLines={2}
              ellipsizeMode="tail"
              accessibilityLiveRegion="polite"
              testID="voice-error-text"
            >
              {voiceError}
            </Text>
          )}
          <VoiceButton
            modelStatus={modelStatus}
            recordingStatus={recordingStatus}
            onPressIn={voiceStartRecording}
            onPressOut={voiceStopRecording}
            onPress={handleVoicePress}
          />
        </RNView>
      )}

      {keyboardVisible && !sessionStopped && streamStatus === "connected" && (
        <SpecialKeysToolbar
          onSendInput={handleSendInput}
          ctrlActive={ctrlActive}
          onCtrlToggle={handleCtrlToggle}
        />
      )}

      {showDownloadSheet && (
        <ModelDownloadSheet
          isDownloading={modelStatus === "downloading"}
          isInitializing={modelStatus === "initializing"}
          downloadProgress={downloadProgress}
          downloadStage={downloadStage}
          onDownload={initModel}
          onCancel={handleDownloadSheetCancel}
          error={modelStatus !== "ready" ? voiceError : null}
        />
      )}

      {streamStatus === "reconnecting" && (
        <RNView
          style={[styles.reconnectOverlay, { backgroundColor: Colors[colorScheme].overlay }]}
          pointerEvents="box-only"
          accessibilityRole="alert"
          accessibilityLabel="Reconnecting to session"
        >
          <ActivityIndicator
            size="large"
            color={Colors[colorScheme].tint}
            accessibilityLabel="Reconnecting to session"
          />
          <Text style={[styles.reconnectText, { color: Colors[colorScheme].onOverlay }]}>
            {wsReconnectAttempts > 0
              ? `Reconnecting\u2026 (attempt ${wsReconnectAttempts}/3)`
              : "Reconnecting\u2026"}
          </Text>
        </RNView>
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
  goBackButton: {
    paddingHorizontal: 24,
    paddingVertical: 12,
    borderRadius: 8,
    borderWidth: 1,
    marginTop: 12,
  },
  buttonText: {
    fontSize: 16,
    fontWeight: "600",
  },
  reconnectOverlay: {
    position: "absolute",
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    justifyContent: "center",
    alignItems: "center",
    zIndex: 2,
  },
  reconnectText: {
    fontSize: 16,
    fontWeight: "600",
    marginTop: 12,
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
  voiceRow: {
    flexDirection: "row",
    justifyContent: "flex-end",
    alignItems: "center",
    paddingHorizontal: 12,
    paddingVertical: 6,
  },
  voiceErrorText: {
    fontSize: 12,
    marginRight: 8,
    flexShrink: 1,
  },
});
