import { expect, test, type Page } from '@playwright/test';

const NOW = new Date('2026-10-19T12:00:00Z');

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

/** Confirms the open dialog and returns the body `POST /commands` sent. */
async function confirm(page: Page, label: string): Promise<unknown> {
  const dialog = page.getByRole('dialog');
  const request = page.waitForRequest(
    (r) => r.method() === 'POST' && r.url().endsWith('/api/v1/commands'),
  );
  await dialog.getByRole('button', { name: label, exact: true }).click();
  await expect(dialog).toBeHidden();
  return (await request).postDataJSON();
}

test('a maintainer pauses a queue and cancels a run, each confirmed with its exact JSON', async ({
  page,
}) => {
  await page.goto('/pools');
  const queue = page.getByRole('region', { name: 'default' });
  await queue.getByRole('button', { name: 'Pause queue default' }).click();
  const dialog = page.getByRole('dialog', { name: 'Pause queue default?' });
  await expect(dialog.getByRole('region', { name: 'Command JSON' })).toHaveText(
    JSON.stringify(
      { type: 'pause', target: { scope: 'queue', name: 'default' } },
      null,
      2,
    ),
  );
  // Going back sends nothing.
  await dialog.getByRole('button', { name: 'Go back' }).click();
  await expect(dialog).toBeHidden();
  await expect(queue.getByTestId('pause-state')).toHaveCount(0);

  await queue.getByRole('button', { name: 'Pause queue default' }).click();
  expect(await confirm(page, 'Pause')).toEqual({
    type: 'pause',
    target: { scope: 'queue', name: 'default' },
  });
  await expect(queue.getByTestId('pause-state')).toHaveText('pause sent');

  // The whole factory, from the header.
  await page
    .getByRole('banner')
    .getByRole('button', { name: 'Pause the whole factory' })
    .click();
  expect(await confirm(page, 'Pause')).toEqual({
    type: 'pause',
    target: { scope: 'global' },
  });

  // Cancel the live run.
  await page.goto('/runs/run_01JAC0RELEASEDRAFT00000000');
  await page.getByRole('button', { name: 'Cancel run' }).click();
  expect(await confirm(page, 'Cancel run')).toEqual({
    type: 'cancel',
    target: { task: 'tsk_01JAC0RELEASEDRAFT000000000' },
  });
  await expect(page.getByTestId('run-control-notice')).toContainText(
    'Cancel accepted',
  );
  await expect(page.getByRole('button', { name: 'Cancel run' })).toHaveCount(0);
});

test('a reader sees no controls', async ({ page, context, baseURL }) => {
  await context.addCookies([
    { name: 'factory-mock-session', value: 'reader', url: baseURL! },
  ]);
  await page.goto('/pools');
  await expect(page.getByTestId('session-login')).toHaveText('octocat');
  await expect(page.getByTestId('read-only')).toHaveText('read-only');
  await expect(page.getByTestId('queue')).toHaveCount(4);
  await expect(page.getByTestId('pause-controls')).toHaveCount(0);

  await page.goto('/runs/run_01JAC0RELEASEDRAFT00000000');
  await expect(page.getByTestId('run-state')).toHaveText('running');
  await expect(page.getByTestId('run-controls')).toHaveCount(0);

  await page.goto('/catalog');
  await expect(page.getByTestId('ruleset')).toHaveCount(2);
  await expect(
    page.getByRole('button', { name: 'Resync catalog' }),
  ).toHaveCount(0);
  await expect(page.getByTestId('pause-controls')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Evaluate' })).toBeVisible();
});
