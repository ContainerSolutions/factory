import { http, HttpResponse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import { examples } from './examples';
import { mockSession, problem } from './session';

type Command = Schemas['Command'];

const SCOPES = ['global', 'provider', 'queue', 'workitem'];

// The example's event id: the mock answers it for every command that becomes an event.
const exampleEvent = (examples.get('postCommand') as Schemas['CommandAccepted'])
  .event!;

function invalid(field: string, message: string): Response {
  return problem(400, 'Bad Request', 'invalid', `${field}: ${message}`, [
    { field, message },
  ]);
}

/**
 * Checks a command's shape as the dispatcher does (api-server.md §Commands), and says which
 * commands become an event: pause and resume only on a work item.
 */
function check(cmd: Command): Response | { event: boolean } {
  const target = cmd.target ?? {};
  switch (cmd.type) {
    case 'pause':
    case 'resume':
      if (!target.scope || !SCOPES.includes(target.scope)) {
        return invalid('target.scope', 'global, provider, queue or workitem');
      }
      if (target.scope !== 'global' && !target.name) {
        return invalid('target.name', `required for scope ${target.scope}`);
      }
      return { event: target.scope === 'workitem' };
    case 'cancel':
    case 'redispatch':
      if (!target.task) return invalid('target.task', 'required');
      if (cmd.args?.harness && !cmd.args.harness.name) {
        return invalid('args.harness.name', 'required');
      }
      return { event: true };
    case 'dispatch':
      if (!cmd.args?.event) return invalid('args.event', 'required');
      return { event: true };
    case 'resync-catalog':
      return { event: false };
    default:
      return invalid(
        'type',
        'pause, resume, cancel, redispatch, dispatch or resync-catalog',
      );
  }
}

/**
 * `POST /commands`: 401 without a session, 403 for a reader (only the maintainers Group may send
 * commands), 400 for a malformed command, else 202 with the example's event id when the command
 * becomes an event.
 */
export const commandHandlers: HttpHandler[] = [
  http.post(`${API_BASE}/commands`, async ({ request, cookies }) => {
    const session = mockSession(cookies);
    if (session === 'signed-out') {
      return problem(401, 'Unauthorized', 'unauthenticated', 'no session');
    }
    if (session === 'reader') {
      return problem(
        403,
        'Forbidden',
        'forbidden',
        'commands need the maintainers Group',
      );
    }
    const cmd = (await request.json().catch(() => null)) as Command | null;
    if (!cmd || typeof cmd !== 'object') return invalid('body', 'not JSON');
    const result = check(cmd);
    if (result instanceof Response) return result;
    const accepted: Schemas['CommandAccepted'] = { type: cmd.type };
    if (result.event) accepted.event = exampleEvent;
    return HttpResponse.json<Schemas['CommandAccepted']>(accepted, {
      status: 202,
    });
  }),
];
