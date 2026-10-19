/** An RFC 3339 time as `2026-10-16 09:30:00 UTC`; the input itself when it doesn't parse. */
export function formatTime(at: string | Date | undefined): string {
  if (at === undefined) return '';
  const t = typeof at === 'string' ? Date.parse(at) : at.getTime();
  if (Number.isNaN(t)) return String(at);
  return new Date(t)
    .toISOString()
    .replace('T', ' ')
    .replace(/\.\d+Z$/, ' UTC');
}

/** A state or input value as text: strings as they are, anything else as JSON. */
export function formatValue(value: unknown): string {
  if (typeof value === 'string') return value;
  return JSON.stringify(value) ?? String(value);
}

/** The pill tone for a status word, so each status reads the same everywhere. */
export type Tone = 'neutral' | 'info' | 'success' | 'warn' | 'danger';

const TONES: Record<string, Tone> = {
  // Lanes and state values.
  'needs-human': 'danger',
  blocked: 'danger',
  running: 'info',
  planning: 'info',
  drafting: 'info',
  done: 'success',
  merged: 'success',
  published: 'success',
  approved: 'success',
  triaged: 'success',
  // Task phases and run ends.
  Pending: 'warn',
  Leased: 'info',
  Running: 'info',
  Succeeded: 'success',
  completed: 'success',
  Failed: 'danger',
  failed: 'danger',
  Cancelling: 'warn',
  Cancelled: 'neutral',
  // Rail verdicts.
  Allowed: 'success',
  Suppressed: 'danger',
  Deferred: 'warn',
  // Approval phases.
  Granted: 'success',
  Denied: 'danger',
  Expired: 'neutral',
};

export function toneOf(status: string | undefined): Tone {
  return (status && TONES[status]) || 'neutral';
}
