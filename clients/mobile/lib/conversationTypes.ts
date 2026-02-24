export interface ConversationTurn {
  origin: "user" | "ai" | "system" | "tool";
  text: string;
  at: Date;
  metadata: Record<string, string>;
  isOptimistic?: boolean;
}
