// ResponseVersion coordinates async responses so only the most recently
// started call — or nothing, after an explicit invalidation — may apply
// state. It prevents a slow older response from overwriting the result of a
// newer call (for example a 30s poll landing after a post-mutation refresh).
export class ResponseVersion {
  private current = 0;

  // Marks the start of a call and returns its version token.
  begin(): number {
    this.current += 1;
    return this.current;
  }

  // Reports whether a call started with `started` is still the latest one.
  isCurrent(started: number): boolean {
    return started === this.current;
  }

  // Invalidates every in-flight call without starting a new one (used when
  // the guarded state is reset or torn down, e.g. on sign-out).
  invalidate(): void {
    this.current += 1;
  }
}
