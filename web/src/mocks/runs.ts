import { http, HttpResponse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import { examples } from './examples';
import { exampleResponse } from './handlers';
import { operations } from './operations.gen';
import { workItemDetails } from './workitems';

type Detail = Schemas['RunDetail'];

// The contract's own example: the finished triage of compliance-framework/docs#42.
const example = examples.get('getRun') as Detail;

/** The mock runs whose workspace uploaded an environment log. */
const ENVIRONMENT_LOG_RUNS = new Set(['run_01JAC0PIPELINETRIAGE000000']);

function withEnvironmentLog(run: Schemas['Run']): Schemas['Run'] {
  if (!ENVIRONMENT_LOG_RUNS.has(run.metadata.name)) return run;
  return {
    ...run,
    status: {
      ...run.status,
      environmentLogRef: `environment-logs/${run.metadata.name}.log`,
    },
  };
}

/**
 * Every run of the mock work items with the effects of its task (effects name the task, not the
 * run); the contract's example as it is.
 */
export const runDetails: readonly Detail[] = [
  example,
  ...workItemDetails.flatMap((detail) =>
    detail.runs
      .filter((run) => run.metadata.name !== example.run.metadata.name)
      .map(
        (run): Detail => ({
          run: withEnvironmentLog(run),
          effects: detail.effects.filter((e) => e.spec.task === run.spec.task),
        }),
      ),
  ),
];

function notFound(what: string): Response {
  return HttpResponse.json<Schemas['Problem']>(
    {
      type: 'about:blank',
      title: 'Not Found',
      status: 404,
      code: 'not-found',
      detail: `${what} not found`,
    },
    {
      status: 404,
      headers: { 'Content-Type': 'application/problem+json' },
    },
  );
}

const transcriptOp = operations.find((op) => op.id === 'getTranscript')!;
const environmentLogOp = operations.find(
  (op) => op.id === 'getEnvironmentLog',
)!;

/**
 * `GET /runs/{id}` per mock run (404 for an unknown one), `GET /runs/{id}/transcript` with
 * the contract's example transcript for a run that has a `transcriptRef` (404 otherwise), and
 * `GET /runs/{id}/environment-log` likewise for an `environmentLogRef`.
 * Registered before the example handlers, which they override.
 */
export const runHandlers: HttpHandler[] = [
  http.get<{ id: string }>(`${API_BASE}/runs/:id`, ({ params }) => {
    const detail = runDetails.find((d) => d.run.metadata.name === params.id);
    return detail
      ? HttpResponse.json<Detail>(detail)
      : notFound(`run ${params.id}`);
  }),
  http.get<{ id: string }>(`${API_BASE}/runs/:id/transcript`, ({ params }) => {
    const detail = runDetails.find((d) => d.run.metadata.name === params.id);
    if (!detail?.run.status?.transcriptRef) {
      return notFound(`transcript of run ${params.id}`);
    }
    return exampleResponse(transcriptOp, examples.get('getTranscript'));
  }),
  http.get<{ id: string }>(
    `${API_BASE}/runs/:id/environment-log`,
    ({ params }) => {
      const detail = runDetails.find((d) => d.run.metadata.name === params.id);
      if (!detail?.run.status?.environmentLogRef) {
        return notFound(`environment log of run ${params.id}`);
      }
      return exampleResponse(
        environmentLogOp,
        examples.get('getEnvironmentLog'),
      );
    },
  ),
];
