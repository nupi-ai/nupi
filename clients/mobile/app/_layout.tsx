import FontAwesome from "@expo/vector-icons/FontAwesome";
import {
  DarkTheme,
  DefaultTheme,
  ThemeProvider,
} from "@react-navigation/native";
import { useFonts } from "expo-font";
import { Stack } from "expo-router";
import * as SplashScreen from "expo-splash-screen";
import { useEffect } from "react";
import "react-native-reanimated";

import { useColorScheme } from "@/components/useColorScheme";
import { ConnectionProvider } from "@/lib/ConnectionContext";
import { NotificationProvider } from "@/lib/NotificationContext";
import { VoiceProvider } from "@/lib/VoiceContext";
import InAppNotificationBanner from "@/components/InAppNotification";

export { ErrorBoundary } from "expo-router";

export const unstable_settings = {
  initialRouteName: "(tabs)",
};

SplashScreen.preventAutoHideAsync();

export default function RootLayout() {
  const [loaded, error] = useFonts({
    SpaceMono: require("../assets/fonts/SpaceMono-Regular.ttf"),
    ...FontAwesome.font,
  });

  useEffect(() => {
    if (error) throw error;
  }, [error]);

  useEffect(() => {
    if (loaded) {
      SplashScreen.hideAsync();
    }
  }, [loaded]);

  if (!loaded) {
    return null;
  }

  return <RootLayoutNav />;
}

function RootLayoutNav() {
  const colorScheme = useColorScheme();

  return (
    <ConnectionProvider>
      <NotificationProvider>
        <VoiceProvider>
          <ThemeProvider value={colorScheme === "dark" ? DarkTheme : DefaultTheme}>
            <Stack>
              <Stack.Screen name="(tabs)" options={{ headerShown: false }} />
              <Stack.Screen
                name="scan"
                options={{ presentation: "modal", title: "Scan QR Code" }}
              />
              <Stack.Screen
                name="session/[id]"
                options={{ title: "Terminal", headerBackTitle: "Sessions" }}
              />
            </Stack>
            <InAppNotificationBanner />
          </ThemeProvider>
        </VoiceProvider>
      </NotificationProvider>
    </ConnectionProvider>
  );
}
