import {
  Pressable,
  type PressableProps,
  type StyleProp,
  type ViewStyle,
} from "react-native";

import { useThemeColors } from "@/components/useColorScheme";

export type ButtonVariant = "primary" | "danger" | "outline" | "unstyled";

interface ButtonProps extends Omit<PressableProps, "style"> {
  variant?: ButtonVariant;
  color?: string;
  style?: StyleProp<ViewStyle>;
  pressedOpacity?: number;
}

function getVariantStyle(
  variant: ButtonVariant,
  color: string
): ViewStyle | undefined {
  if (variant === "outline") {
    return { borderWidth: 1, borderColor: color };
  }

  if (variant === "unstyled") {
    return undefined;
  }

  return { backgroundColor: color };
}

export function Button({
  variant = "primary",
  color,
  style,
  pressedOpacity = 0.7,
  accessibilityRole,
  ...props
}: ButtonProps) {
  const colors = useThemeColors();

  const resolvedColor =
    color ?? (variant === "danger" ? colors.danger : colors.tint);
  const variantStyle = getVariantStyle(variant, resolvedColor);

  return (
    <Pressable
      {...props}
      accessibilityRole={accessibilityRole ?? "button"}
      style={({ pressed }) => [
        style,
        variantStyle,
        { opacity: pressed ? pressedOpacity : 1 },
      ]}
    />
  );
}
