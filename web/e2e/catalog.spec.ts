import { expect, test } from '@playwright/test';

const NOW = new Date('2026-10-19T12:00:00Z');

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

test('catalog: commit, providers, RuleSets with their rules, and a dry run with why', async ({
  page,
}) => {
  await page.goto('/catalog');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Catalog');
  await expect(page.getByTestId('catalog-sha')).toHaveText('9f1e2d3c4b5a');
  await expect(page.getByTestId('catalog-status')).toHaveText('loaded');

  // Providers and their health.
  const providers = page.getByTestId('provider');
  await expect(providers.getByRole('rowheader')).toHaveText([
    'github-public',
    'github-enterprise',
    'slack-ccf',
  ]);
  await expect(page.getByTestId('provider-health')).toHaveText([
    /^ok/,
    /^degraded\s*rate limited until 12:30 UTC/,
    /^ok/,
  ]);

  // The docs RuleSet's selection: refs ∪ selector ∪ inline, minus exclude, params merged.
  const docs = page.getByRole('region', { name: 'docs' });
  await expect(docs.getByTestId('rule-name')).toHaveText([
    'docs/label-docs',
    'merge-approved',
    'triage-outside-issues',
  ]);
  await expect(
    docs.getByRole('region', { name: 'Params of merge-approved' }),
  ).toContainText('"mergeMethod": "rebase"');
  await expect(page.getByTestId('taskspec')).toHaveCount(3);
  await expect(page.getByTestId('catalog-queue')).toHaveCount(4);

  // Dry run a recorded event of the escalated work item.
  await page.getByRole('combobox', { name: 'Recorded on work item' }).click();
  await page
    .getByRole('option', { name: 'compliance-framework/api#310' })
    .click();
  const eventPick = page.getByRole('combobox', { name: 'Event' });
  await expect(eventPick).not.toHaveAttribute('aria-disabled', 'true');
  await eventPick.click();
  await page
    .getByRole('option', {
      name: 'pr.review.submitted · compliance-framework/api#310 · 2026-10-17 14:02:00 UTC',
    })
    .click();
  await expect(page.getByTestId('dry-run-workitem')).toHaveValue(
    'wi_01JAC0ESCALATED0000000000000',
  );
  await expect(page.getByTestId('dry-run-event')).toHaveValue(
    /"kind": "Event"/,
  );
  await page.getByRole('button', { name: 'Evaluate' }).click();

  const evaluation = page.getByTestId('evaluation');
  await expect(evaluation).toContainText('wi_01JAC0ESCALATED0000000000000');
  await expect(evaluation.getByTestId('match')).toHaveCount(2);
  await expect(evaluation.getByTestId('decision-verdict')).toHaveText([
    'Allowed',
    'Allowed',
  ]);
  await page
    .getByRole('button', {
      name: 'Why: docs / triage-outside-issues (action 1)',
    })
    .click();
  const why = page.getByRole('dialog', {
    name: 'Why: docs / triage-outside-issues',
  });
  await expect(why.getByTestId('why-catalog-sha')).toHaveText(
    '9f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6',
  );
  await expect(why.getByRole('region', { name: 'where inputs' })).toContainText(
    '"actor.tier": "contributor"',
  );

  // No secret ref is ever rendered.
  for (const secret of [
    'github-public-webhook',
    'ccf-factory-app',
    'bot-token',
  ]) {
    await expect(page.locator('body')).not.toContainText(secret);
  }
});
