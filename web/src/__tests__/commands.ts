import { flushPromises } from '@vue/test-utils';
import { http } from 'msw';
import type { Schemas } from '@/api/client';
import { server } from '@/mocks/node';

/**
 * Records the body of every `POST /commands` while the mock handlers answer it, so a spec can
 * assert the exact command a control sent.
 */
export function recordCommands(): Schemas['Command'][] {
  const bodies: Schemas['Command'][] = [];
  server.use(
    http.post('/api/v1/commands', async ({ request }) => {
      bodies.push((await request.clone().json()) as Schemas['Command']);
      // No response: the next handler (src/mocks/commands.ts) answers.
      return undefined;
    }),
  );
  return bodies;
}

/** Waits until `check` passes, flushing the event loop between tries. */
export async function until(check: () => boolean, tries = 50): Promise<void> {
  for (let i = 0; i < tries && !check(); i++) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
}

/** Clicks the button labelled `label` in the open dialog, and waits for what it sends. */
export async function confirmDialog(label: string): Promise<void> {
  const button = [
    ...document.body.querySelectorAll<HTMLButtonElement>(
      '[role="dialog"] button',
    ),
  ].find((b) => b.textContent?.trim() === label);
  if (!button) throw new Error(`no "${label}" button in an open dialog`);
  button.click();
  await flushPromises();
}
