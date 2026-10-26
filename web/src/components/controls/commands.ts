import { describeProblem, type Schemas } from '@/api/client';

export type Command = Schemas['Command'];

/** What pause and resume act on. There is no pool scope. */
export type PauseScope = NonNullable<Schemas['CommandTarget']['scope']>;

/** `pause` or `resume` of one scope: the whole factory (`global`), or a named provider, queue or work item. */
export function pauseCommand(
  type: 'pause' | 'resume',
  scope: PauseScope,
  name?: string,
): Command {
  return scope === 'global'
    ? { type, target: { scope } }
    : { type, target: { scope, name } };
}

/** Cancels a task's active run. */
export function cancelCommand(task: string): Command {
  return { type: 'cancel', target: { task } };
}

/** Another run of a finished task, on another harness (and model) when `harness` is given. */
export function redispatchCommand(
  task: string,
  harness?: { name: string; model?: string },
): Command {
  const command: Command = { type: 'redispatch', target: { task } };
  const name = harness?.name.trim();
  if (name) {
    const model = harness?.model?.trim();
    command.args = {
      harness: model ? { name, config: { model } } : { name },
    };
  }
  return command;
}

/** Re-reads the catalog from its repo now. */
export function resyncCatalogCommand(): Command {
  return { type: 'resync-catalog' };
}

/** The key of a pause scope, as the dispatcher names its kill switches: `global`, `<scope>:<name>`. */
export function pauseKey(scope: PauseScope, name?: string): string {
  return scope === 'global' ? 'global' : `${scope}:${name ?? ''}`;
}

/** What a pause scope is, for a dialog's text: `the whole factory`, `queue triage`. */
export function describeScope(scope: PauseScope, name?: string): string {
  return scope === 'global'
    ? 'the whole factory'
    : `${scope === 'workitem' ? 'work item' : scope} ${name ?? ''}`.trim();
}

/**
 * Why a command (or another admin request) was refused, as one line. A 403 says who may do
 * `what`: send commands, by default.
 */
export function describeCommandFailure(
  problem: unknown,
  response: Response,
  what = 'send commands',
): string {
  const p = (problem ?? {}) as Partial<Schemas['Problem']>;
  const fields = (p.errors ?? [])
    .filter((e) => typeof e?.field === 'string')
    .map((e) => `${e.field}: ${e.message}`);
  const base = describeProblem(problem, response);
  const detail = fields.length ? `${base} (${fields.join('; ')})` : base;
  switch (response.status) {
    case 401:
      return 'Your session has expired: sign in again.';
    case 403:
      return `Only members of the maintainers Group can ${what}. The server said: ${detail}`;
    default:
      return detail;
  }
}
