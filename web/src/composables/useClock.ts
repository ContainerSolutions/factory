import { inject, type InjectionKey } from 'vue';

/** The current time. Injected so views that depend on it (the board's Done window) are testable. */
export type Clock = () => Date;

export const clockKey: InjectionKey<Clock> = Symbol('clock');

const systemClock: Clock = () => new Date();

/** The provided clock, or the system clock. Provide one with `app.provide(clockKey, …)`. */
export function useClock(): Clock {
  return inject(clockKey, systemClock);
}
