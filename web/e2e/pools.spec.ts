import { expect, test } from '@playwright/test';

// The mock queues and agents (src/mocks/pools.ts) are dated around this instant.
const NOW = new Date('2026-10-19T12:00:00Z');

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

test('queues with why their tasks wait, and pools with their agents, live', async ({
  page,
}) => {
  await page.goto('/pools');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText(
    'Queues and pools',
  );

  // Queues in the scheduler's order, each pending task with its reason.
  const queues = page.getByTestId('queue');
  await expect(queues.getByRole('heading', { level: 3 })).toHaveText([
    'review',
    'triage',
    'default',
    'nightly',
  ]);
  const defaultQueue = page.getByRole('region', { name: 'default' });
  await expect(defaultQueue.getByTestId('queue-depth')).toHaveText('3 pending');
  await expect(defaultQueue.getByTestId('queue-running')).toHaveText('1 of 2');
  await expect(defaultQueue.getByTestId('pending-reason')).toHaveText([
    'agent-throttled',
    'no-capable-agent',
    'lending',
  ]);
  await expect(defaultQueue.getByTestId('pending-why').nth(1)).toContainText(
    'No agent in the pool offers',
  );
  await expect(
    page.getByRole('region', { name: 'triage' }).getByTestId('pending-reason'),
  ).toHaveText('budget');

  // Pools, and their agents with lending, throttle windows and harness offers.
  await expect(
    page.getByTestId('pool').getByRole('heading', { level: 3 }),
  ).toHaveText(['maintainer-agents', 'ci-ephemeral']);
  const laptop = page.getByRole('article', { name: 'agt_gusfcarvalho-mbp-01' });
  await expect(laptop.getByTestId('agent-phase')).toHaveText('Busy');
  await expect(laptop.getByTestId('agent-heartbeat')).toContainText('20 s ago');
  await expect(laptop.getByTestId('agent-today')).toHaveText(
    /4 of 30 runs · 430,200 of 3,000,000 tokens/,
  );
  await expect(laptop.getByTestId('agent-lending')).toContainText(
    'default, triage, review',
  );
  await expect(laptop.getByTestId('harness-offer')).toHaveCount(2);
  await expect(laptop.getByTestId('default-model')).toHaveCount(2);

  const throttled = page.getByRole('article', { name: 'agt_octocat-linux-02' });
  await expect(throttled.getByTestId('agent-phase')).toHaveText('Throttled');
  await expect(throttled.getByTestId('agent-throttle')).toHaveText(
    /until 2026-10-19 13:30:00 UTC\s*\(in 1 h 30 min\)/,
  );

  // The mock stream brings the offline builder back.
  const builder = page.getByRole('article', { name: 'agt_ccf-bot-builder-03' });
  await expect(page.getByTestId('stream-status')).toHaveText('live');
  await expect(builder.getByTestId('agent-phase')).toHaveText('Ready');
  await expect(builder.getByTestId('agent-heartbeat')).toContainText('on time');
  // The contract's older agent.updated example didn't overwrite the laptop.
  await expect(laptop.getByTestId('agent-phase')).toHaveText('Busy');

  // No secret ref is ever rendered.
  await expect(page.locator('body')).not.toContainText('join-token');
  await expect(page.locator('body')).not.toContainText('api-key');
});
