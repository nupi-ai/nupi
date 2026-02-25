import { defineConfig } from "vitest/config";
import path from "path";

export default defineConfig({
  resolve: {
    alias: {
      "@/": path.resolve(__dirname, "./") + "/",
      "react-native": path.resolve(__dirname, "./lib/__tests__/__mocks__/react-native.ts"),
      "react-native-reanimated": path.resolve(__dirname, "./lib/__tests__/__mocks__/react-native-reanimated.ts"),
      "whisper.rn": path.resolve(__dirname, "./lib/__tests__/__mocks__/whisper.rn.ts"),
    },
  },
  test: {
    environment: "jsdom",
  },
});
