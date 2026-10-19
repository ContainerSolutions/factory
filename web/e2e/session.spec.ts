import { expect, test } from '@playwright/test';

test('without a session, a page sends you to sign in and back', async ({
  page,
  context,
  baseURL,
}) => {
  await context.addCookies([
    { name: 'factory-mock-session', value: 'signed-out', url: baseURL! },
  ]);
  await page.goto('/catalog');
  await expect(page).toHaveURL(/\/login\?returnTo=\/catalog$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Sign in');
  await expect(page.getByRole('navigation', { name: 'Main' })).toHaveCount(0);

  // The dev server's mock sign-in stands in for GitHub and the callback.
  await page.getByRole('link', { name: 'Sign in with GitHub' }).click();
  await expect(page).toHaveURL(/\/catalog$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Catalog');
  await expect(page.getByTestId('session-login')).toHaveText('gusfcarvalho');
});

test('signing out ends the session', async ({ page }) => {
  await page.goto('/pools');
  await expect(page.getByTestId('session-login')).toHaveText('gusfcarvalho');
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);
  // Still signed out: the board sends you back to sign in.
  await page.getByRole('link', { name: 'CCF Factory' }).click();
  await expect(page).toHaveURL(/\/login$/);
});
