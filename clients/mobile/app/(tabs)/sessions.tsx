import { useCallback, useEffect, useRef, useState } from "react";
import {
  ActivityIndicator,
  FlatList,
  RefreshControl,
  StyleSheet,
  View as RNView,
} from "react-native";
import { router } from "expo-router";

import { ConnectionErrorView } from "@/components/ConnectionErrorView";
import type { Session } from "@/lib/gen/nupi_pb";
import Colors from "@/constants/designTokens";
import { useColorScheme } from "@/components/useColorScheme";
import { Text, View } from "@/components/Themed";
import { useConnection } from "@/lib/ConnectionContext";
import { SessionItem } from "@/components/SessionItem";
import type { MappedError } from "@/lib/errorMessages";
import { mapConnectionError } from "@/lib/errorMessages";

export default function SessionsScreen() {
  const colorScheme = useColorScheme() ?? "light";
  const colors = Colors[colorScheme];
  const { status, client, reconnecting, reconnectAttempts } = useConnection();

  const [sessions, setSessions] = useState<Session[]>([]);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<MappedError | null>(null);
  const mountedRef = useRef(true);
  const hasFetchedRef = useRef(false);

  const fetchSessions = useCallback(async () => {
    if (!client) return;
    try {
      const response = await client.sessions.listSessions({});
      if (mountedRef.current) {
        setSessions(response.sessions);
        setError(null);
      }
    } catch (err) {
      if (mountedRef.current) {
        setError(mapConnectionError(err));
      }
    }
  }, [client]);

  // Fetch sessions on mount and when connection becomes "connected"
  useEffect(() => {
    mountedRef.current = true;

    if (status === "connected" && client) {
      setError(null);
      setLoading(true);
      hasFetchedRef.current = true;
      fetchSessions().finally(() => {
        if (mountedRef.current) setLoading(false);
      });
    }

    return () => {
      mountedRef.current = false;
    };
  }, [status, client, fetchSessions]);

  const handleRefresh = useCallback(async () => {
    if (status !== "connected") return;
    setRefreshing(true);
    await fetchSessions();
    if (mountedRef.current) setRefreshing(false);
  }, [status, fetchSessions]);

  const handleRetry = useCallback(() => {
    if (status !== "connected") return;
    setError(null);
    setLoading(true);
    fetchSessions().finally(() => {
      if (mountedRef.current) setLoading(false);
    });
  }, [status, fetchSessions]);

  // Not connected state
  if (status !== "connected" && !hasFetchedRef.current && !reconnecting) {
    return (
      <View style={styles.centered}>
        <Text style={styles.emptyText}>
          Connect to nupid first using the Home tab.
        </Text>
      </View>
    );
  }

  // Loading state (initial fetch)
  if (loading && sessions.length === 0) {
    return (
      <View style={styles.centered}>
        <ActivityIndicator
          size="large"
          color={colors.tint}
          accessibilityLabel="Loading sessions"
        />
      </View>
    );
  }

  // Error state with retry (or reconnect hint when disconnected)
  if (error && sessions.length === 0 && !reconnecting) {
    return (
      <View style={styles.centered}>
        <ConnectionErrorView
          variant="retry-only"
          error={error}
          onRetry={status === "connected" ? handleRetry : undefined}
          useConnectionRetry={false}
          fallbackText="Reconnect on the Home tab to retry."
          accessibilityLabel="Retry loading sessions"
          accessibilityHint="Attempts to fetch the session list again"
          buttonTestID="retry-sessions-button"
          messageStyle={[styles.errorText, { color: colors.danger }]}
          fallbackStyle={styles.emptyText}
          buttonStyle={styles.retryButton}
          buttonTextStyle={[styles.retryButtonText, { color: colors.background }]}
        />
      </View>
    );
  }

  const isOffline = status !== "connected" && sessions.length > 0;

  return (
    <View style={styles.container}>
      {/* Reconnecting/Offline banner — black text on yellow for WCAG contrast */}
      {(isOffline || reconnecting) && (
        <RNView
          style={[styles.offlineBanner, { backgroundColor: colors.warning }]}
          accessibilityRole="alert"
        >
          <Text style={[styles.offlineBannerText, { color: colors.onWarning }]}>
            {reconnecting
              ? reconnectAttempts > 0
                ? `Reconnecting\u2026 (attempt ${reconnectAttempts}/3)`
                : "Reconnecting\u2026"
              : status === "connecting"
                ? "Reconnecting\u2026"
                : "Offline \u2014 showing last known sessions"}
          </Text>
        </RNView>
      )}

      {/* Error banner — white text on red for WCAG contrast */}
      {error && sessions.length > 0 && !reconnecting && (
        <RNView
          style={[styles.offlineBanner, { backgroundColor: colors.danger }]}
          accessibilityRole="alert"
        >
          <Text style={[styles.offlineBannerText, { color: colors.onDanger }]}>
            {error.message}
          </Text>
        </RNView>
      )}

      <FlatList
        data={sessions}
        accessibilityRole="list"
        keyExtractor={(item) => item.id}
        renderItem={({ item }) => (
          <SessionItem
            session={item}
            onPress={() => router.push(`/session/${item.id}`)}
          />
        )}
        refreshControl={
          <RefreshControl
            refreshing={refreshing}
            onRefresh={handleRefresh}
            enabled={status === "connected"}
            tintColor={colors.tint}
          />
        }
        ListEmptyComponent={
          <RNView style={styles.centered}>
            <Text style={styles.emptyText}>
              No active sessions. Start a session on your desktop.
            </Text>
          </RNView>
        }
        contentContainerStyle={
          sessions.length === 0 ? styles.centered : styles.listContent
        }
      />
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
  },
  centered: {
    flex: 1,
    alignItems: "center",
    justifyContent: "center",
    padding: 20,
  },
  listContent: {
    paddingVertical: 8,
  },
  emptyText: {
    fontSize: 16,
    textAlign: "center",
    opacity: 0.7,
  },
  errorText: {
    fontSize: 14,
    textAlign: "center",
    marginBottom: 16,
  },
  retryButton: {
    paddingHorizontal: 24,
    paddingVertical: 12,
    borderRadius: 12,
  },
  retryButtonText: {
    fontSize: 16,
    fontWeight: "600",
  },
  offlineBanner: {
    paddingHorizontal: 16,
    paddingVertical: 8,
    alignItems: "center",
  },
  offlineBannerText: {
    fontSize: 13,
    fontWeight: "500",
  },
});
