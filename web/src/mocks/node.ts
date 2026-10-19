import { setupServer } from 'msw/node';
import { catalogHandlers } from './catalog';
import { commandHandlers } from './commands';
import { handlers } from './handlers';
import { poolHandlers } from './pools';
import { runHandlers } from './runs';
import { sessionHandlers } from './session';
import { workItemHandlers } from './workitems';

// The fixtures come first: MSW uses the first handler that matches. No stream here: MSW mocks
// SSE through EventSource, which Node lacks (specs inject a fake EventSource instead).
export const server = setupServer(
  ...workItemHandlers,
  ...runHandlers,
  ...poolHandlers,
  ...catalogHandlers,
  ...sessionHandlers,
  ...commandHandlers,
  ...handlers,
);
