import { useEffect, useMemo, useRef, useState } from "react";
import type { LanguageInfo } from "../api";
import { theme } from "../designTokens";
import { useLanguagePreference } from "../hooks/useLanguagePreference";
import * as styles from "./voicePanelStyles";

export function LanguageSelector() {
  const { languages, language, loading, error, listLanguages, setLanguage } =
    useLanguagePreference();
  const [search, setSearch] = useState("");

  // Fetch languages once on mount
  const hasFetched = useRef(false);
  useEffect(() => {
    if (!hasFetched.current) {
      hasFetched.current = true;
      listLanguages();
    }
  }, [listLanguages]);

  const filtered = useMemo(() => {
    if (!search.trim()) return languages;
    const q = search.toLowerCase();
    return languages.filter(
      (l) =>
        l.native_name.toLowerCase().includes(q) ||
        l.english_name.toLowerCase().includes(q) ||
        l.iso1.toLowerCase().includes(q),
    );
  }, [languages, search]);

  const selectedLabel = useMemo(() => {
    if (!language) return "Auto-detect (no preference)";
    const found = languages.find(
      (l) => l.iso1.toLowerCase() === language.toLowerCase(),
    );
    if (found) return `${found.native_name} (${found.english_name})`;
    return language;
  }, [language, languages]);

  return (
    <section style={styles.section}>
      <h3 style={styles.sectionHeading}>Language Preference</h3>
      <p style={styles.descriptionText}>
        Choose a language for voice interactions. When set, all voice commands
        and responses will use this language. Leave unset for auto-detection.
      </p>

      <div style={styles.currentLanguageRow}>
        <span style={{ ...styles.labelText, marginRight: "8px" }}>
          Current:
        </span>
        <span style={{ color: language ? theme.text.success : theme.text.secondary }}>
          {selectedLabel}
        </span>
        {language && (
          <button
            onClick={() => setLanguage(null)}
            style={styles.clearLanguageButton}
          >
            Clear
          </button>
        )}
      </div>

      <input
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Search languages..."
        aria-label="Search languages"
        style={{ ...styles.textInput, width: "100%", marginBottom: "8px" }}
      />

      {loading && <p style={styles.infoText}>Loading languages...</p>}
      {error && <p style={styles.warningText}>Error: {error}</p>}

      {!loading && filtered.length > 0 && (
        <div
          role="listbox"
          aria-label="Available languages"
          style={styles.languageListBox}
        >
          {filtered.map((lang: LanguageInfo) => {
            const isSelected =
              !!language &&
              language.toLowerCase() === lang.iso1.toLowerCase();
            return (
              <button
                key={lang.iso1}
                role="option"
                aria-selected={isSelected}
                onClick={() => setLanguage(lang.iso1)}
                style={{
                  ...styles.languageOptionBase,
                  backgroundColor: isSelected ? theme.bg.languageSelected : theme.bg.transparent,
                  color: isSelected ? theme.text.languageSelected : theme.text.secondaryButton,
                }}
              >
                {lang.native_name} ({lang.english_name})
                <span style={styles.languageIsoLabel}>{lang.iso1}</span>
              </button>
            );
          })}
        </div>
      )}

      {!loading && languages.length > 0 && filtered.length === 0 && (
        <p style={styles.infoText}>No languages match your search.</p>
      )}
    </section>
  );
}
