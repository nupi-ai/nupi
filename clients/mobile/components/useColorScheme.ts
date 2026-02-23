import Colors from "@/constants/designTokens";
import { useColorScheme as useNativeColorScheme } from "react-native";

export function useColorScheme() {
  return useNativeColorScheme();
}

export function useThemeColors(fallback: keyof typeof Colors = "light") {
  const colorScheme = useColorScheme() ?? fallback;
  return Colors[colorScheme];
}
