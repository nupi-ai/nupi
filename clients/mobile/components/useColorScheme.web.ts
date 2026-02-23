import Colors from "@/constants/designTokens";

export function useColorScheme() {
  return "light" as const;
}

export function useThemeColors(fallback: keyof typeof Colors = "light") {
  const colorScheme = useColorScheme() ?? fallback;
  return Colors[colorScheme];
}
