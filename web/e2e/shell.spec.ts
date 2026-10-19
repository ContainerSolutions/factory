import { expect, test } from '@playwright/test';

test('the shell renders with navigation over the mock API', async ({
  page,
}) => {
  await page.goto('/');
  const nav = page.getByRole('navigation', { name: 'Main' });
  await expect(nav.getByRole('link')).toHaveText(['Board', 'Pools', 'Catalog']);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Board');
  // getMe's example, served by MSW.
  await expect(page.getByTestId('session-login')).toHaveText('gusfcarvalho');

  await nav.getByRole('link', { name: 'Pools' }).click();
  await expect(page).toHaveURL(/\/pools$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText(
    'Queues and pools',
  );
});
