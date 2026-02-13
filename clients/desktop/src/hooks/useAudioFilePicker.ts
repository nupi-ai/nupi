import { useCallback, useState } from "react";
import { open, save } from "@tauri-apps/plugin-dialog";

export interface AudioFilePickerState {
  inputPath: string | null;
  outputPath: string | null;
  disablePlayback: boolean;
  setDisablePlayback: (value: boolean) => void;
  pickInput: () => Promise<void>;
  pickOutput: () => Promise<void>;
  clearOutput: () => void;
}

export function useAudioFilePicker(): AudioFilePickerState {
  const [inputPath, setInputPath] = useState<string | null>(null);
  const [outputPath, setOutputPath] = useState<string | null>(null);
  const [disablePlayback, setDisablePlayback] = useState(true);

  const pickInput = useCallback(async () => {
    try {
      const selected = await open({
        multiple: false,
        directory: false,
        filters: [
          { name: "Audio", extensions: ["wav"] },
          { name: "All Files", extensions: ["*"] },
        ],
      });

      if (!selected) {
        return;
      }

      setInputPath(selected);
    } catch (error) {
      console.error("Failed to open audio picker", error);
    }
  }, []);

  const pickOutput = useCallback(async () => {
    try {
      const saved = await save({
        defaultPath: "nupi-playback.wav",
        filters: [{ name: "WAV", extensions: ["wav"] }],
      });

      if (saved) {
        setOutputPath(saved);
        setDisablePlayback(false);
      }
    } catch (error) {
      console.error("Failed to open save dialog", error);
    }
  }, []);

  const clearOutput = useCallback(() => {
    setOutputPath(null);
    setDisablePlayback(true);
  }, []);

  return {
    inputPath,
    outputPath,
    disablePlayback,
    setDisablePlayback,
    pickInput,
    pickOutput,
    clearOutput,
  };
}
