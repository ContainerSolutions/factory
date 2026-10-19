import { setupWorker } from 'msw/browser';
import { handlers } from './handlers';
import { poolHandlers } from './pools';
import { runHandlers } from './runs';
import { streamHandlers } from './stream';
import { workItemHandlers } from './workitems';

// The fixtures come first: MSW uses the first handler that matches.
export const worker = setupWorker(
  ...streamHandlers(),
  ...workItemHandlers,
  ...runHandlers,
  ...poolHandlers,
  ...handlers,
);
