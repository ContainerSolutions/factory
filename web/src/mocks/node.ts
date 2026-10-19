import { setupServer } from 'msw/node';
import { handlers } from './handlers';
import { runHandlers } from './runs';
import { workItemHandlers } from './workitems';

// The fixtures come first: MSW uses the first handler that matches. No stream here: MSW mocks
// SSE through EventSource, which Node lacks (specs inject a fake EventSource instead).
export const server = setupServer(
  ...workItemHandlers,
  ...runHandlers,
  ...handlers,
);
