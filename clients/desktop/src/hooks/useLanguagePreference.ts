import { useCallback, useEffect, useState } from "react";
import { api, toErrorMessage, type LanguageInfo } from "../api";

export function useLanguagePreference() {
  const [languages, setLanguages] = useState<LanguageInfo[]>([]);
  const [language, setLanguageState] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Load persisted language on mount
  useEffect(() => {
    api.language.get()
      .then((lang) => setLanguageState(lang ?? null))
      .catch((err) => console.error("Failed to load language:", err));
  }, []);

  const listLanguages = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await api.language.list();
      setLanguages(result);
    } catch (err) {
      setError(toErrorMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  const setLanguage = useCallback(async (iso1: string | null) => {
    setError(null);
    try {
      await api.language.set(iso1);
      setLanguageState(iso1 ? iso1.trim().toLowerCase() || null : null);
    } catch (err) {
      setError(toErrorMessage(err));
    }
  }, []);

  return { languages, language, loading, error, listLanguages, setLanguage };
}
