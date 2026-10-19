import { expect, test } from '@playwright/test';

// The mock work items (src/mocks/workitems.ts) are dated around this instant.
const NOW = new Date('2026-10-19T12:00:00Z');

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

test('work item → run: live progress over the mock stream', async ({
  page,
}) => {
  await page.goto('/workitems/wi_01JAC0RELEASE000000000000000');
  await page
    .getByRole('link', { name: 'run_01JAC0RELEASEDRAFT00000000' })
    .click();
  await expect(page).toHaveURL(/\/runs\/run_01JAC0RELEASEDRAFT00000000$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Run');
  await expect(page.getByTestId('run-state')).toHaveText('running');
  await expect(page.getByTestId('run-agent')).toHaveText(
    'agt_gusfcarvalho-mbp-01',
  );
  await expect(page.getByTestId('run-owner')).toHaveText(
    'github-public:gusfcarvalho',
  );

  // The ring buffer first, then the mock stream's frames as they arrive.
  const progress = page.getByTestId('progress-event');
  await expect(page.getByTestId('stream-status')).toHaveText('live');
  await expect(progress).toHaveCount(5);
  await expect(progress).toHaveText([
    /cloning platform\/release-notes/,
    /read repos\.yaml\s*turn 2/,
    /read CHANGELOG\.md/,
    /edit docs\/release\/v1\.9\.0\.md/,
    /drafting the release notes/,
  ]);
  await expect(page.getByTestId('harness')).toHaveText(
    'The effective config is reported with the Result.',
  );
  await expect(page.getByTestId('usage')).toContainText('18,200');
});

test('a finished run: Result, effects and the transcript, loaded on demand', async ({
  page,
}) => {
  await page.goto('/runs/run_01JAB3M3P4Q5R6S7T8V9W0X1YZ');
  await expect(page.getByTestId('run-state')).toHaveText('ended');
  await expect(page.getByTestId('stream-status')).toHaveCount(0);
  await expect(page.getByTestId('progress-event')).toHaveCount(2);
  await expect(page.getByTestId('harness')).toContainText('gpt-5-codex');
  await expect(page.getByTestId('effect').getByRole('link')).toHaveAttribute(
    'href',
    'https://github.com/compliance-framework/docs/issues/42#issuecomment-2401',
  );
  await expect(page.getByRole('region', { name: 'Result JSON' })).toContainText(
    '"outcome": "triaged"',
  );

  await page.getByRole('button', { name: 'Load transcript' }).click();
  const turns = page.getByTestId('transcript-turn');
  await expect(turns.getByRole('button')).toHaveText([
    /Session/,
    /Turn 1\b/,
    /Turn 3\b/,
    /Turn 17\b/,
  ]);
  const turn17 = page.getByRole('button', { name: /Turn 17/ });
  await expect(turn17).toHaveAttribute('aria-expanded', 'false');
  await turn17.click();
  await expect(turn17).toHaveAttribute('aria-expanded', 'true');
  await expect(page.getByTestId('transcript-entry')).toHaveText([
    /assistant\s*Triaged: missing --profile flag; wrote out\/result\.json\./,
  ]);
});

test('an unknown run says so', async ({ page }) => {
  await page.goto('/runs/run_missing');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText(
    'Run not found',
  );
});
