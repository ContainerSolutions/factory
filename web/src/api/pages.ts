import { describeProblem } from './client';

/**
 * How many pages of one list are followed. Queues, pools and providers are catalog objects and
 * agents are a team's machines, so one page is the norm; this only bounds a server that keeps
 * paginating.
 */
export const MAX_PAGES = 20;

type Page<T> = { items: T[]; nextCursor?: string };

export type PageResult<T> = {
  data?: Page<T>;
  error?: unknown;
  response: Response;
};

/** Every page of a list, following `nextCursor`; a failed page fails the whole list. */
export async function collect<T>(
  fetchPage: (cursor?: string) => Promise<PageResult<T>>,
): Promise<{ items: T[] } | { error: string }> {
  const items: T[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < MAX_PAGES; page++) {
    const { data, error, response } = await fetchPage(cursor);
    if (!data) return { error: describeProblem(error, response) };
    items.push(...data.items);
    cursor = data.nextCursor;
    if (!cursor) break;
  }
  return { items };
}

/** The query of one page: the cursor, when there is one. */
export const pageQuery = (cursor?: string) => ({
  params: { query: cursor ? { cursor } : {} },
});
