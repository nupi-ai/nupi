import { useRef } from "react";
import {
  ActivityIndicator,
  Platform,
  Pressable,
  StyleSheet,
  View as RNView,
} from "react-native";

import { Text } from "@/components/Themed";
import { useColorScheme } from "@/components/useColorScheme";
import Colors from "@/constants/Colors";
import {
  MODEL_SIZE_MB,
  GGML_MODEL_SIZE_MB,
  COREML_SIZE_MB,
  isModelDownloaded,
  isCoreMLDownloaded,
} from "@/lib/whisper";
import type { DownloadStage } from "@/lib/VoiceContext";

/** Format MB as human-readable size — shows GB for values ≥ 1024. */
function formatSize(mb: number): string {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
}

interface ModelDownloadSheetProps {
  isDownloading: boolean;
  isInitializing: boolean;
  downloadProgress: number;
  downloadStage: DownloadStage;
  onDownload: () => void;
  onCancel: () => void;
  error: string | null;
}

function getDownloadingLabel(stage: DownloadStage): string {
  if (stage === "extracting") {
    return "Extracting CoreML model";
  }
  if (stage === "coreml") {
    return `Downloading CoreML acceleration (${formatSize(COREML_SIZE_MB)})`;
  }
  return `Downloading voice model (${formatSize(GGML_MODEL_SIZE_MB)})`;
}

/** Compute the actual remaining download size based on what's already on disk. */
function getRemainingDownloadMB(): number {
  const needsGgml = !isModelDownloaded();
  const needsCoreml = Platform.OS === "ios" && !isCoreMLDownloaded();
  let size = 0;
  if (needsGgml) size += GGML_MODEL_SIZE_MB;
  if (needsCoreml) size += COREML_SIZE_MB;
  return size || MODEL_SIZE_MB; // fallback to total if both present (shouldn't happen)
}

export function ModelDownloadSheet({
  isDownloading,
  isInitializing,
  downloadProgress,
  downloadStage,
  onDownload,
  onCancel,
  error,
}: ModelDownloadSheetProps) {
  const colorScheme = useColorScheme() ?? "dark";
  // Compute remaining size once when the sheet first renders (before download
  // starts). Stored in a ref so re-renders during download don't re-read the
  // filesystem (file existence may be in flux) and the displayed value stays
  // consistent with the initial prompt the user saw.
  const remainingMBRef = useRef<number | null>(null);
  if (remainingMBRef.current === null) {
    remainingMBRef.current = getRemainingDownloadMB();
  }
  const remainingMB = remainingMBRef.current;

  return (
    <RNView
      style={[
        styles.overlay,
        { backgroundColor: Colors[colorScheme].overlay },
      ]}
      accessibilityLabel="Voice model download prompt"
    >
      {/* Backdrop tap to dismiss — standard bottom sheet UX pattern */}
      <Pressable onPress={onCancel} accessibilityRole="button" accessibilityLabel="Dismiss" style={styles.backdrop} />
      <RNView
        style={[
          styles.sheet,
          { backgroundColor: Colors[colorScheme].surface },
        ]}
      >
        <Text
          style={[styles.title, { color: Colors[colorScheme].text }]}
          accessibilityRole="header"
        >
          Download Voice Model
        </Text>

        <Text style={[styles.description, { color: Colors[colorScheme].text }]}>
          {isInitializing && !isDownloading
            ? "Voice model downloaded — preparing for first use. This may take a moment."
            : <>
                Download voice model for offline speech recognition.{" "}
                {Platform.OS === "ios"
                  ? `~${formatSize(remainingMB)} total (voice model + CoreML acceleration).`
                  : `~${formatSize(remainingMB)}.`}{" "}
                The model is stored locally and only needs to be downloaded once.
              </>
          }
        </Text>

        {error && (
          <Text
            style={[styles.errorText, { color: Colors[colorScheme].danger }]}
            numberOfLines={2}
            ellipsizeMode="tail"
            accessibilityLiveRegion="assertive"
          >
            {error}
          </Text>
        )}

        {isDownloading && (
          <RNView style={styles.progressContainer}>
            <ActivityIndicator
              size="small"
              color={Colors[colorScheme].tint}
              accessibilityLabel="Downloading voice model"
            />
            <Text
              style={[styles.progressText, { color: Colors[colorScheme].text }]}
            >
              {getDownloadingLabel(downloadStage)}... {downloadProgress}%
            </Text>
          </RNView>
        )}

        {isInitializing && !isDownloading && (
          <RNView style={styles.progressContainer}>
            <ActivityIndicator
              size="small"
              color={Colors[colorScheme].tint}
              accessibilityLabel="Initializing voice model"
            />
            <Text
              style={[styles.progressText, { color: Colors[colorScheme].text }]}
            >
              Initializing voice model...
            </Text>
          </RNView>
        )}

        <RNView style={styles.buttons}>
          {!isDownloading && !isInitializing && (
            <Pressable
              onPress={onDownload}
              style={({ pressed }) => [
                styles.downloadButton,
                {
                  backgroundColor: Colors[colorScheme].tint,
                  opacity: pressed ? 0.7 : 1,
                },
              ]}
              accessibilityRole="button"
              accessibilityLabel={`Download voice model, approximately ${formatSize(remainingMB)}`}
              accessibilityHint="Starts downloading the voice recognition model"
              testID="model-download-button"
            >
              <Text
                style={[
                  styles.downloadButtonText,
                  { color: Colors[colorScheme].background },
                ]}
              >
                Download (~{formatSize(remainingMB)})
              </Text>
            </Pressable>
          )}

          <Pressable
            onPress={onCancel}
            style={({ pressed }) => [
              styles.cancelButton,
              {
                borderColor: Colors[colorScheme].separator,
                opacity: pressed ? 0.7 : 1,
              },
            ]}
            accessibilityRole="button"
            accessibilityLabel={isDownloading || isInitializing ? "Hide progress" : "Close download prompt"}
            testID="model-download-cancel"
          >
            <Text style={[styles.cancelButtonText, { color: Colors[colorScheme].text }]}>
              {isDownloading || isInitializing ? "Hide" : "Not Now"}
            </Text>
          </Pressable>
        </RNView>
      </RNView>
    </RNView>
  );
}

const styles = StyleSheet.create({
  overlay: {
    position: "absolute",
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    justifyContent: "flex-end",
    zIndex: 10,
  },
  backdrop: {
    flex: 1,
  },
  sheet: {
    borderTopLeftRadius: 16,
    borderTopRightRadius: 16,
    padding: 24,
    paddingBottom: 36,
  },
  title: {
    fontSize: 18,
    fontWeight: "700",
    marginBottom: 12,
  },
  description: {
    fontSize: 14,
    lineHeight: 20,
    marginBottom: 16,
  },
  errorText: {
    fontSize: 13,
    marginBottom: 12,
  },
  progressContainer: {
    flexDirection: "row",
    alignItems: "center",
    gap: 8,
    marginBottom: 16,
  },
  progressText: {
    fontSize: 13,
  },
  buttons: {
    gap: 12,
  },
  downloadButton: {
    paddingVertical: 14,
    borderRadius: 10,
    alignItems: "center",
  },
  downloadButtonText: {
    fontSize: 16,
    fontWeight: "600",
  },
  cancelButton: {
    paddingVertical: 14,
    borderRadius: 10,
    alignItems: "center",
    borderWidth: 1,
  },
  cancelButtonText: {
    fontSize: 16,
    fontWeight: "500",
  },
});
