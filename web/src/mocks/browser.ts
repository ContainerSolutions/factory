import { setupWorker } from 'msw/browser';
import { handlers } from './handlers';
import { workItemHandlers } from './workitems';

// The work-item fixtures come first: MSW uses the first handler that matches.
export const worker = setupWorker(...workItemHandlers, ...handlers);
