import { QueryClient } from "@tanstack/react-query";

function createMobileQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        gcTime: 5 * 60_000,
        retry: 2,
        retryDelay: (attempt) => Math.min(500 * (2 ** (attempt - 1)), 4_000),
        refetchOnReconnect: true,
        refetchOnWindowFocus: false,
      },
    },
  });
}

export const mobileQueryClient = createMobileQueryClient();
