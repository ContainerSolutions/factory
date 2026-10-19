import { setupServer } from 'msw/node';
import { handlers } from './handlers';
import { workItemHandlers } from './workitems';

// The work-item fixtures come first: MSW uses the first handler that matches.
export const server = setupServer(...workItemHandlers, ...handlers);
