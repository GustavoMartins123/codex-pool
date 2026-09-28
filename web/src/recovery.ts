export type RecoveryLinkState =
  | { phase: "checking" }
  | { phase: "ready"; expiresAt: Date }
  | { phase: "unavailable" };

export type RecoveryEvent = {
  type: "status" | "recheck";
  valid: boolean;
  expiresAt?: string | null;
};

// The backend is the single authority: every status or recheck answer fully
// replaces the local state, so consumed or replaced links hide the form even
// while a countdown is still running.
export function recoveryReducer(_state: RecoveryLinkState, event: RecoveryEvent): RecoveryLinkState {
  if (!event.valid || !event.expiresAt) return { phase: "unavailable" };
  const expiresAt = new Date(event.expiresAt);
  if (Number.isNaN(expiresAt.getTime())) return { phase: "unavailable" };
  return { phase: "ready", expiresAt };
}

// Milliseconds until the server-provided deadline; 0 means it already passed.
export function recoveryDeadlineMs(expiresAt: Date, now: number = Date.now()): number {
  const remaining = expiresAt.getTime() - now;
  return remaining > 0 ? remaining : 0;
}
