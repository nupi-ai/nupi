import { useCallback, useEffect, useRef, useState } from "react";
import {
  ActivityIndicator,
  FlatList,
  Pressable,
  RefreshControl,
  StyleSheet,
  View as RNView,
} from "react-native";
import { router } from "expo-router";
import { ConnectError, Code } from "@connectrpc/connect";

import type { Session } from "@/lib/gen/nupi_pb";
import Colors from "@/constants/Colors";
import { useColorScheme } from "@/components/useColorScheme";
import { Text, View } from "@/components/Themed";
import { useConnection } from "@/lib/ConnectionContext";
import { SessionItem } from "@/components/SessionItem";

function mapSessionsError(err: unknown): string {
  if (err instanceof ConnectError) {
    switch (err.code) {
      case Code.Unauthenticated:
        return "Session expired \u2014 please re-pair.";
      case Code.PermissionDenied:
        return "Permission denied \u2014 please re-pair.";
      case Code.Unavailable:
        return "Cannot reach nupid.";
      default:
        return `Error: ${err.message}`;
    }
  }
  if (err instanceof Error) {
    return `Connection error: ${err.message}`;
  }
  return "An unexpected error occurred.";
}

export default function SessionsScreen() {
  const colorScheme = useColorScheme() ?? "light";
  const colors = Colors[colorScheme];
  const { status, client } = useConnection();

  const [sessions, setSessions] = useState<Session[]>([]);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
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
        setError(mapSessionsError(err));
      }
    }
  }, [client]);

  // Fetch sessions on mount and when connection becomes "connected"
  useEffect(() => {
    mountedRef.current = true;

    if (status === "connected" && client) {
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
  if (status !== "connected" && !hasFetchedRef.current) {
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
  if (error && sessions.length === 0) {
    return (
      <View style={styles.centered}>
        <Text style={[styles.errorText, { color: colors.danger }]}>
          {error}
        </Text>
        {status === "connected" ? (
          <Pressable
            style={({ pressed }) => [
              styles.retryButton,
              { backgroundColor: colors.tint, opacity: pressed ? 0.7 : 1 },
            ]}
            onPress={handleRetry}
            accessibilityRole="button"
            accessibilityLabel="Retry loading sessions"
            accessibilityHint="Attempts to fetch the session list again"
          >
            <Text style={[styles.retryButtonText, { color: colors.background }]}>
              Retry
            </Text>
          </Pressable>
        ) : (
          <Text style={styles.emptyText}>
            Reconnect on the Home tab to retry.
          </Text>
        )}
      </View>
    );
  }

  const isOffline = status !== "connected" && sessions.length > 0;

  return (
    <View style={styles.container}>
      {/* Offline banner — black text on yellow for WCAG contrast */}
      {isOffline && (
        <RNView style={[styles.offlineBanner, { backgroundColor: colors.warning }]}>
          <Text style={[styles.offlineBannerText, { color: "#000" }]}>
            {status === "connecting"
              ? "Reconnecting\u2026"
              : "Offline \u2014 showing last known sessions"}
          </Text>
        </RNView>
      )}

      {/* Error banner — white text on red for WCAG contrast */}
      {error && sessions.length > 0 && (
        <RNView style={[styles.offlineBanner, { backgroundColor: colors.danger }]}>
          <Text style={[styles.offlineBannerText, { color: "#fff" }]}>
            {error}
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
