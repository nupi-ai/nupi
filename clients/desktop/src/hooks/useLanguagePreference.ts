import { useCallback, useEffect, useState } from "react";
import { invoke } from "@tauri-apps/api/core";

export interface LanguageInfo {
  iso1: string;
  bcp47: string;
  english_name: string;
  native_name: string;
}

export function useLanguagePreference() {
  const [languages, setLanguages] = useState<LanguageInfo[]>([]);
  const [language, setLanguageState] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Load persisted language on mount
  useEffect(() => {
    invoke<string | null>("get_language")
      .then((lang) => setLanguageState(lang ?? null))
      .catch((err) => console.error("Failed to load language:", err));
  }, []);

  const listLanguages = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await invoke<LanguageInfo[]>("list_languages");
      setLanguages(result);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
    } finally {
      setLoading(false);
    }
  }, []);

  const setLanguage = useCallback(async (iso1: string | null) => {
    setError(null);
    try {
      await invoke("set_language", { language: iso1 });
      setLanguageState(iso1 ? iso1.trim().toLowerCase() || null : null);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
    }
  }, []);

  return { languages, language, loading, error, listLanguages, setLanguage };
}
