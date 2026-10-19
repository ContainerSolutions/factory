import { afterEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '@/mocks/node';
import { api, onUnauthorized } from './client';

const unauthorized = () =>
  HttpResponse.json({ title: 'Unauthorized', status: 401 }, { status: 401 });

let remove: (() => void) | undefined;

afterEach(() => remove?.());

describe('onUnauthorized', () => {
  it('is called when a request answers 401, until removed', async () => {
    const listener = vi.fn();
    remove = onUnauthorized(listener);
    server.use(http.get('/api/v1/queues', unauthorized));
    await api.GET('/queues');
    expect(listener).toHaveBeenCalledTimes(1);

    remove();
    await api.GET('/queues');
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('ignores the auth routes and other statuses', async () => {
    const listener = vi.fn();
    remove = onUnauthorized(listener);
    server.use(http.get('/api/v1/auth/me', unauthorized));
    await api.GET('/auth/me');
    await api.GET('/queues');
    expect(listener).not.toHaveBeenCalled();
  });
});
