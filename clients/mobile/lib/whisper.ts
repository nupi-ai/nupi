import { Paths, File, Directory } from "expo-file-system";
import { createDownloadResumable, type DownloadProgressData } from "expo-file-system/legacy";
import { Platform } from "react-native";
import { initWhisper, type WhisperContext } from "whisper.rn";
import { unzip } from "react-native-zip-archive";

/** Strip file:// prefix — react-native-zip-archive expects filesystem paths, not URIs. */
function toPath(uri: string): string {
  return uri.replace(/^file:\/\//, "");
}

/** Track download progress stalls — resets on every progress callback. */
function createStallDetector(timeoutMs: number) {
  let timer: ReturnType<typeof setTimeout> | null = null;
  let rejectFn: ((err: Error) => void) | null = null;
  const promise = new Promise<never>((_, reject) => { rejectFn = reject; });
  const reset = () => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => {
      rejectFn?.(new Error(`Download stalled — no progress for ${timeoutMs / 1000}s`));
    }, timeoutMs);
  };
  const clear = () => {
    if (timer) { clearTimeout(timer); timer = null; }
  };
  return { promise, reset, clear };
}

const MODEL_FILENAME = "ggml-medium-q8_0.bin";
const GGML_MODEL_SIZE_MB = 500;
// Minimum expected size in bytes (~490MB). Anything smaller is likely truncated/corrupt.
const MODEL_MIN_BYTES = 490 * 1024 * 1024;
// Pinned to specific commit — `resolve/main/` tracks HEAD and can break if upstream
// reorganizes files. Update hash when upgrading to a newer model version.
const MODEL_REVISION = "5359861c739e955e79d9a303bcbc70fb988958b1";
const MODEL_URL =
  `https://huggingface.co/ggerganov/whisper.cpp/resolve/${MODEL_REVISION}/ggml-medium-q8_0.bin`;

// CoreML model assets for Neural Engine acceleration on iOS.
// whisper.cpp auto-discovers these by looking for `ggml-medium-encoder.mlmodelc/`
// adjacent to the .bin file (strips `-q8_0` quantization suffix automatically).
const COREML_FILENAME = "ggml-medium-encoder.mlmodelc.zip";
const COREML_DIR_NAME = "ggml-medium-encoder.mlmodelc";
const COREML_URL =
  `https://huggingface.co/ggerganov/whisper.cpp/resolve/${MODEL_REVISION}/ggml-medium-encoder.mlmodelc.zip`;
const COREML_SIZE_MB = 568;
// Minimum expected zip size in bytes (~540MB). Anything smaller is likely truncated/corrupt.
const COREML_MIN_BYTES = 540 * 1024 * 1024;
const DOWNLOAD_STALL_TIMEOUT_MS = 60_000;
const UNZIP_TIMEOUT_MS = 120_000;
const INIT_TIMEOUT_MS = 30_000;

/** Platform-aware total download size: ggml model + CoreML assets on iOS. */
const MODEL_SIZE_MB = Platform.OS === "ios" ? GGML_MODEL_SIZE_MB + COREML_SIZE_MB : GGML_MODEL_SIZE_MB;

function getModelDir(): Directory {
  return new Directory(Paths.document, "models/");
}

/** Returns `file://` URI for the model file. Use `toPath()` before passing to native APIs that expect filesystem paths. */
function getModelUri(): string {
  return new File(getModelDir(), MODEL_FILENAME).uri;
}

export { MODEL_SIZE_MB };

export function isModelDownloaded(): boolean {
  const file = new File(getModelDir(), MODEL_FILENAME);
  return file.exists && file.size >= MODEL_MIN_BYTES;
}

type DownloadProgressCallback = (progress: number) => void;

export async function downloadModel(
  onProgress?: DownloadProgressCallback
): Promise<void> {
  const dir = getModelDir();
  if (!dir.exists) {
    dir.create({ intermediates: true });
  }

  const fileUri = new File(dir, MODEL_FILENAME).uri;

  const stall = createStallDetector(DOWNLOAD_STALL_TIMEOUT_MS);

  const downloadResumable = createDownloadResumable(
    MODEL_URL,
    fileUri,
    {},
    (data: DownloadProgressData) => {
      stall.reset();
      if (data.totalBytesExpectedToWrite > 0) {
        const percent = Math.round(
          (data.totalBytesWritten / data.totalBytesExpectedToWrite) * 100
        );
        onProgress?.(percent);
      }
    }
  );

  // Track whether download phase completed — catch block should only call
  // pauseAsync() when the download is still in-flight, not for post-download
  // errors (size validation). Matches downloadCoreML() pattern.
  let downloadComplete = false;

  try {
    stall.reset();
    // Attach .catch to download promise — if the stall timeout wins the race
    // and downloadAsync() later rejects (abort from pauseAsync), Hermes would
    // emit unhandledRejection without this handler.
    const downloadPromise = downloadResumable.downloadAsync();
    downloadPromise.catch(() => {});
    const result = await Promise.race([
      downloadPromise,
      stall.promise,
    ]);
    stall.clear();
    if (!result) {
      throw new Error("Download returned no result");
    }

    // Basic size validation — catches truncated downloads or corrupt responses.
    // onProgress(100) is deferred until after validation so the user doesn't
    // see 100% for a truncated download that will immediately error.
    const downloaded = new File(dir, MODEL_FILENAME);
    if (!downloaded.exists) {
      throw new Error("Download completed but model file not found on disk");
    }
    if (downloaded.size < MODEL_MIN_BYTES) {
      const actualSize = downloaded.size;
      downloaded.delete();
      throw new Error(
        `Downloaded model is too small (${Math.round(actualSize / 1024 / 1024)}MB), expected ~${GGML_MODEL_SIZE_MB}MB`
      );
    }

    onProgress?.(100);
    downloadComplete = true;
  } catch (err) {
    stall.clear();
    if (!downloadComplete) {
      try { await downloadResumable.pauseAsync(); } catch {}
    }
    // Delete partial file on failure — prevents corrupted model load on next launch.
    try {
      const partial = new File(dir, MODEL_FILENAME);
      if (partial.exists) {
        partial.delete();
      }
    } catch {
      // Ignore cleanup errors
    }
    throw err;
  }
}

export function deleteModel(): void {
  const file = new File(getModelDir(), MODEL_FILENAME);
  if (file.exists) {
    file.delete();
  }
}

export { COREML_SIZE_MB, GGML_MODEL_SIZE_MB };

// Minimum expected sizes for CoreML validation files. Catches 0-byte stubs
// from disk-full during extraction that would silently fall back to CPU.
const COREML_WEIGHT_MIN_BYTES = 100 * 1024 * 1024; // ~100MB — weight.bin is the largest file

/**
 * Checks if CoreML model assets are downloaded and contain required files
 * with non-trivial sizes. whisper.cpp expects `ggml-medium-encoder.mlmodelc/`
 * with model.mil, coremldata.bin, and weights/weight.bin inside.
 */
export function isCoreMLDownloaded(): boolean {
  const dir = getModelDir();
  const coremlDir = new Directory(dir, COREML_DIR_NAME);
  if (!coremlDir.exists) return false;

  // Verify key files exist and have non-trivial sizes inside the .mlmodelc directory
  const modelMil = new File(coremlDir, "model.mil");
  const coremlData = new File(coremlDir, "coremldata.bin");
  const weightsDir = new Directory(coremlDir, "weights");
  const weightBin = new File(weightsDir, "weight.bin");

  if (!modelMil.exists || !coremlData.exists || !weightBin.exists) return false;

  // Size validation — weight.bin is the bulk of CoreML data. 0-byte or tiny
  // files indicate extraction was interrupted (e.g., disk full).
  return modelMil.size > 0 && coremlData.size > 0 && weightBin.size >= COREML_WEIGHT_MIN_BYTES;
}

/**
 * Downloads and extracts CoreML model assets for iOS Neural Engine acceleration.
 * Downloads the zip from HuggingFace, extracts to models dir, deletes the zip.
 */
export async function downloadCoreML(
  onProgress?: DownloadProgressCallback
): Promise<void> {
  const dir = getModelDir();
  if (!dir.exists) {
    dir.create({ intermediates: true });
  }

  const zipUri = new File(dir, COREML_FILENAME).uri;

  const stall = createStallDetector(DOWNLOAD_STALL_TIMEOUT_MS);

  const downloadResumable = createDownloadResumable(
    COREML_URL,
    zipUri,
    {},
    (data: DownloadProgressData) => {
      stall.reset();
      if (data.totalBytesExpectedToWrite > 0) {
        const percent = Math.round(
          (data.totalBytesWritten / data.totalBytesExpectedToWrite) * 100
        );
        onProgress?.(percent);
      }
    }
  );

  // Track whether download phase completed — catch block should only call
  // pauseAsync() when the download is still in-flight, not for post-download
  // errors (unzip timeout, extraction validation).
  let downloadComplete = false;

  try {
    stall.reset();
    // Attach .catch to download promise — if the stall timeout wins the race
    // and downloadAsync() later rejects (abort from pauseAsync), Hermes would
    // emit unhandledRejection without this handler.
    const downloadPromise = downloadResumable.downloadAsync();
    downloadPromise.catch(() => {});
    const result = await Promise.race([
      downloadPromise,
      stall.promise,
    ]);
    stall.clear();
    if (!result) {
      throw new Error("CoreML download returned no result");
    }

    // Size validation — catches truncated downloads before expensive extraction.
    // onProgress(100) is deferred until after validation so the user doesn't
    // see 100% for a truncated download that will immediately error.
    const downloadedZip = new File(dir, COREML_FILENAME);
    if (!downloadedZip.exists) {
      throw new Error("Download completed but CoreML zip not found on disk");
    }
    if (downloadedZip.size < COREML_MIN_BYTES) {
      const actualSize = downloadedZip.size;
      downloadedZip.delete();
      throw new Error(
        `Downloaded CoreML zip is too small (${Math.round(actualSize / 1024 / 1024)}MB), expected ~${COREML_SIZE_MB}MB`
      );
    }

    onProgress?.(100);
    downloadComplete = true;

    // Extract zip to models directory — creates ggml-medium-encoder.mlmodelc/
    // Timeout-guarded: corrupt zip with valid size could cause native unzip
    // to loop/hang. 120s is generous for ~568MB extraction on lower-end devices.
    // Note: unlike downloads (pauseAsync), native unzip() has no cancellation API.
    // On timeout, the extraction may still be running when the catch block deletes
    // the target directory. This race is benign — deletion failures are swallowed
    // by try/catch, and the orphaned unzip either fails (no target) or recreates
    // the directory (re-evaluated by isCoreMLDownloaded on next initModel).
    // Attach .catch to unzip promise — if the timeout wins and unzip() later
    // rejects, Hermes would emit unhandledRejection. Timer is tracked and
    // cleared on success to avoid a 120s orphaned setTimeout.
    const unzipPromise = unzip(toPath(zipUri), toPath(dir.uri));
    unzipPromise.catch(() => {});
    let unzipTimer: ReturnType<typeof setTimeout> | null = null;
    const unzipTimeout = new Promise<never>((_, reject) => {
      unzipTimer = setTimeout(
        () => reject(new Error(`CoreML extraction timed out after ${UNZIP_TIMEOUT_MS / 1000}s`)),
        UNZIP_TIMEOUT_MS,
      );
    });
    try {
      await Promise.race([unzipPromise, unzipTimeout]);
    } finally {
      if (unzipTimer) clearTimeout(unzipTimer);
    }

    // Verify extraction produced all required files — a truncated/corrupt zip
    // could extract partially, causing silent CPU fallback via WHISPER_COREML_ALLOW_FALLBACK.
    if (!isCoreMLDownloaded()) {
      throw new Error("CoreML extraction incomplete — required files missing");
    }

    // Delete the zip after successful extraction. Wrapped in its own
    // try-catch so a transient deletion failure (disk error, file lock)
    // doesn't propagate to the outer catch which would destroy the
    // valid, already-verified .mlmodelc directory.
    try {
      if (downloadedZip.exists) {
        downloadedZip.delete();
      }
    } catch {
      // Orphaned zip wastes ~568MB but CoreML is valid — not fatal.
    }
  } catch (err) {
    stall.clear();
    // Only abort the download if it's still in-flight. Post-download errors
    // (unzip timeout, extraction validation) should not call pauseAsync() —
    // it's a no-op on a completed download but adds async latency and is
    // semantically misleading.
    if (!downloadComplete) {
      try { await downloadResumable.pauseAsync(); } catch {}
    }
    // Clean up partial zip AND any partially-extracted directory on failure.
    // unzip() may have created ggml-medium-encoder.mlmodelc/ before throwing.
    try {
      const partial = new File(dir, COREML_FILENAME);
      if (partial.exists) {
        partial.delete();
      }
    } catch {
      // Ignore cleanup errors
    }
    try {
      const partialDir = new Directory(dir, COREML_DIR_NAME);
      if (partialDir.exists) {
        partialDir.delete();
      }
    } catch {
      // Ignore cleanup errors
    }
    throw err;
  }
}

/** Removes CoreML directory and any orphaned zip (for recovery or settings). */
export function deleteCoreML(): void {
  const dir = getModelDir();
  const coremlDir = new Directory(dir, COREML_DIR_NAME);
  if (coremlDir.exists) {
    coremlDir.delete();
  }
  // Also clean up orphaned zip — post-extraction deletion can fail silently
  // (whisper.ts:230-236), leaving ~568MB on disk permanently.
  const zip = new File(dir, COREML_FILENAME);
  if (zip.exists) {
    zip.delete();
  }
}

async function initWhisperContext(
  modelPath: string
): Promise<WhisperContext> {
  // CoreML with Neural Engine on iOS — requires .mlmodelc assets downloaded
  // alongside the ggml model. useGpu must be false because Metal overrides CoreML.
  // whisper.rn podspec defines WHISPER_COREML_ALLOW_FALLBACK so if CoreML files
  // are missing, init silently falls back to CPU (no crash).
  // On Android: useGpu is ignored, CPU with NEON SIMD is used automatically.
  //
  // Timeout guard: if native whisper.cpp enters an infinite loop on a corrupt
  // model file, the app would be stuck in "initializing" forever. A 30s timeout
  // ensures recovery — the caller deletes the model and prompts re-download.
  const initPromise = initWhisper({
    filePath: toPath(modelPath),
    useGpu: false,
    useCoreMLIos: true,
  });
  let timer: ReturnType<typeof setTimeout> | null = null;
  const timeoutPromise = new Promise<never>((_, reject) => {
    timer = setTimeout(
      () => reject(new Error(`Model initialization timed out after ${INIT_TIMEOUT_MS / 1000}s — model may be corrupt`)),
      INIT_TIMEOUT_MS,
    );
  });
  try {
    const ctx = await Promise.race([initPromise, timeoutPromise]);
    if (timer) clearTimeout(timer);
    return ctx;
  } catch (err) {
    if (timer) clearTimeout(timer);
    // If timeout won the race, native initWhisper() is still in-flight.
    // Attach cleanup to release the WhisperContext if it eventually succeeds —
    // prevents ~500MB native memory leak from orphaned context.
    initPromise.then((ctx) => {
      ctx.release().catch((releaseErr) => {
        console.warn("[whisper] failed to release orphaned WhisperContext after init timeout:", releaseErr);
      });
    }).catch(() => {});
    throw err;
  }
}

class WhisperManager {
  private contextRef: WhisperContext | null = null;
  private initPromise: Promise<WhisperContext> | null = null;

  async init(): Promise<WhisperContext> {
    if (this.contextRef) {
      return this.contextRef;
    }

    // Return shared promise for concurrent callers
    if (this.initPromise) {
      return this.initPromise;
    }

    this.initPromise = (async () => {
      try {
        const modelUri = getModelUri();
        const downloaded = isModelDownloaded();
        if (!downloaded) {
          throw new Error("Whisper model not downloaded");
        }
        if (Platform.OS === "ios" && !isCoreMLDownloaded()) {
          throw new Error("CoreML model assets not downloaded");
        }

        const t0 = performance.now();
        const ctx = await initWhisperContext(modelUri);
        const elapsed = Math.round(performance.now() - t0);
        console.log(
          `[whisper] initWhisperContext completed in ${elapsed}ms (platform=${Platform.OS})`
        );
        this.contextRef = ctx;
        return ctx;
      } finally {
        this.initPromise = null;
      }
    })();

    return this.initPromise;
  }

  getContext(): WhisperContext | null {
    return this.contextRef;
  }
}

export const whisperManager = new WhisperManager();
