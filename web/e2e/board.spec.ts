import { expect, test } from '@playwright/test';

// The mock work items (src/mocks/workitems.ts) are dated around this instant; fixing the clock
// keeps the board's "Done (last 7 days)" lane stable.
const NOW = new Date('2026-10-19T12:00:00Z');

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

test('board → work item → why panel over the mock API', async ({ page }) => {
  await page.goto('/');

  // The lanes, needs-human first.
  const lanes = page.getByRole('region').filter({
    has: page.getByRole('heading', { level: 2 }),
  });
  await expect(lanes.getByRole('heading', { level: 2 })).toHaveText([
    'Needs human 2',
    'Running 2',
    'Waiting 1',
    'Done (last 7 days) 1',
  ]);
  const needsHuman = page.getByRole('region', { name: /Needs human/ });
  await expect(needsHuman.getByRole('link')).toHaveText([
    'compliance-framework/api#310',
    'compliance-framework/docs#57',
  ]);

  // Filter by provider; the filter goes into the URL.
  await page.getByRole('combobox', { name: 'Provider' }).click();
  await page.getByRole('option', { name: 'github-enterprise' }).click();
  await expect(page).toHaveURL(/\?provider=github-enterprise$/);
  await expect(page.getByTestId('workitem-card')).toHaveText([
    /platform\/release-notes#7/,
  ]);
  await page.getByRole('button', { name: 'Clear filters' }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByTestId('workitem-card')).toHaveCount(6);

  // Open the escalated work item.
  await needsHuman
    .getByRole('link', { name: 'compliance-framework/api#310' })
    .click();
  await expect(page).toHaveURL(/\/workitems\/wi_01JAC0ESCALATED0000000000000$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText(
    'compliance-framework/api#310',
  );
  await expect(page.getByTestId('state-keys')).toContainText('needs-human');
  await expect(page.getByTestId('links')).toContainText('slack-ccf');
  await expect(page.getByTestId('timeline-entry')).toHaveCount(6);

  // The why panel of the decision a rail held back, opened from the keyboard.
  const why = page.getByRole('button', {
    name: 'Why: api / implement-review-feedback',
  });
  await why.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', {
    name: 'Why: api / implement-review-feedback',
  });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByTestId('why-ruleset')).toHaveText('api');
  await expect(dialog.getByTestId('why-rule')).toHaveText(
    'implement-review-feedback',
  );
  await expect(
    dialog.getByRole('region', { name: 'where inputs' }),
  ).toContainText('"params.maxImplementRuns": 3');
  await expect(dialog.getByRole('row')).toHaveText([
    /Rail\s*Verdict\s*Reason/,
    /run-cap\s*Suppressed\s*implement ran 3 of 3 times/,
  ]);

  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();

  // The example work item's decision has a task, so its catalog commit is known.
  await page.getByRole('link', { name: '← Board' }).click();
  await page
    .getByRole('link', { name: 'compliance-framework/docs#42' })
    .click();
  await page
    .getByRole('button', { name: 'Why: docs / triage-outside-issues' })
    .click();
  await expect(
    page.getByRole('dialog').getByTestId('why-catalog-sha'),
  ).toHaveText('9f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6');
});

test('an unknown work item says so', async ({ page }) => {
  await page.goto('/workitems/wi_missing');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText(
    'Work item not found',
  );
});
