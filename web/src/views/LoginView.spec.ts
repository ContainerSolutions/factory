import { describe, expect, it } from 'vitest';
import { mountWithApp } from '@/__tests__/mount';
import { useSessionStore } from '@/stores/session';
import LoginView from './LoginView.vue';

describe('LoginView', () => {
  it.each([
    ['/login', '/api/v1/auth/login'],
    [
      '/login?returnTo=/runs/run_1',
      '/api/v1/auth/login?returnTo=%2Fruns%2Frun_1',
    ],
    ['/login?returnTo=//evil.example', '/api/v1/auth/login'],
  ])('at %s, signs in through %s', async (path, href) => {
    const { wrapper } = await mountWithApp(LoginView, { path });
    expect(wrapper.get('h1').text()).toBe('Sign in');
    const link = wrapper.get('a[data-testid="sign-in"]');
    expect(link.text()).toBe('Sign in with GitHub');
    expect(link.attributes('href')).toBe(href);
  });

  it("says when the session couldn't be checked", async () => {
    const { wrapper } = await mountWithApp(LoginView, { path: '/login' });
    const session = useSessionStore();
    session.status = 'error';
    session.error = 'HTTP 502';
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[role="alert"]').text()).toBe(
      "Couldn't check your session: HTTP 502",
    );
  });
});
