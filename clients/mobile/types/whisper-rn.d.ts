// Custom type declarations for the whisper.rn main module.
// Submodule types (whisper.rn/realtime-transcription, adapters) come from
// the package's own lib/typescript/. Re-verify this file when upgrading whisper.rn
// — it shadows any types the package may ship for the main entry point.
declare module "whisper.rn" {
  export interface ContextOptions {
    filePath: string | number;
    coreMLModelAsset?: {
      filename: string;
      assets: string[] | number[];
    };
    isBundleAsset?: boolean;
    useCoreMLIos?: boolean;
    useGpu?: boolean;
    useFlashAttn?: boolean;
  }

  export interface TranscribeOptions {
    language?: string;
    translate?: boolean;
    maxThreads?: number;
    maxLen?: number;
    tokenTimestamps?: boolean;
    offset?: number;
    duration?: number;
    temperature?: number;
    beamSize?: number;
    bestOf?: number;
    prompt?: string;
  }

  export interface TranscribeResult {
    result: string;
    language: string;
    segments: Array<{ text: string; t0: number; t1: number }>;
    isAborted: boolean;
  }

  export interface VadOptions {
    threshold?: number;
    minSpeechDurationMs?: number;
    minSilenceDurationMs?: number;
    maxSpeechDurationS?: number;
    speechPadMs?: number;
    samplesOverlap?: number;
  }

  export class WhisperContext {
    id: number;
    gpu: boolean;
    transcribe(
      filePath: string,
      options?: TranscribeOptions
    ): { stop: () => Promise<void>; promise: Promise<TranscribeResult> };
    transcribeData(
      data: ArrayBuffer,
      options: TranscribeOptions
    ): { stop: () => Promise<void>; promise: Promise<TranscribeResult> };
    release(): Promise<void>;
  }

  export function initWhisper(
    options: ContextOptions
  ): Promise<WhisperContext>;
  export function releaseAllWhisper(): Promise<void>;
}
