import type { Schemas } from '@/api/client';
import { harnessText } from '@/components/catalog/catalog';
import { formatValue, safeURL } from '@/components/shared/format';
import { asRecord } from './board';
import { IN_FLIGHT } from './graph';

type Task = Schemas['Task'];
type Run = Schemas['Run'];

/** One agent task of the work item, for the LLM tasks tab. */
export interface LLMTask {
  task: Task;
  /** Its runs, oldest attempt first. */
  runs: Run[];
  latest?: Run;
  /** Not over yet: Pending, Leased, Running or Cancelling. */
  inFlight: boolean;
  /** `mode` and `target` first, then the other inputs, as text. */
  inputs: [string, string][];
  harness: string;
  outcome?: string;
  summary?: string;
  /** The PRs or issues it produced: the Result's `data.pr`, its artifacts and its effects' URLs. */
  produced: { label: string; url?: string }[];
}

const FIRST_INPUTS = ['mode', 'target'];

function time(at: string | undefined): number {
  const t = at ? Date.parse(at) : NaN;
  return Number.isNaN(t) ? Infinity : t;
}

/** The harness a task runs with: the resolved one (name and model), else the request. */
function harnessOf(task: Task, latest: Run | undefined): string {
  const r = task.spec.harness?.resolved;
  if (r) {
    const parts = [
      r.model,
      r.reasoningEffort ? `effort ${r.reasoningEffort}` : '',
    ]
      .filter(Boolean)
      .join(', ');
    return parts ? `${r.name} (${parts})` : r.name;
  }
  const used = latest?.status?.result?.harness;
  if (used?.name) {
    const model = used.effectiveConfig?.model;
    return typeof model === 'string' && model
      ? `${used.name} (${model})`
      : used.name;
  }
  return harnessText(task.spec.harness?.request) || 'the agent picks';
}

/** Every task of the work item in creation order, with its runs, state and what it produced. */
export function llmTasksOf(detail: Schemas['WorkItemDetail']): LLMTask[] {
  return [...detail.tasks]
    .sort((a, b) => time(a.metadata.createdAt) - time(b.metadata.createdAt))
    .map((task) => {
      const runs = detail.runs
        .filter((r) => r.spec.task === task.metadata.id)
        .sort((a, b) => a.spec.attempt - b.spec.attempt);
      const latest = runs[runs.length - 1];
      const result = latest?.status?.result;
      const inputs = Object.entries(asRecord(task.spec.inputs))
        .sort(([a], [b]) => {
          const ia = FIRST_INPUTS.indexOf(a);
          const ib = FIRST_INPUTS.indexOf(b);
          return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib) || a.localeCompare(b);
        })
        .map(([k, v]): [string, string] => [k, formatValue(v)]);
      const produced: LLMTask['produced'] = [];
      const seen = new Set<string>();
      const add = (label: string, url?: string) => {
        const key = url ?? label;
        if (seen.has(key)) return;
        seen.add(key);
        produced.push(url ? { label, url } : { label });
      };
      for (const r of runs) {
        const pr = asRecord(r.status?.result?.data).pr;
        if (typeof pr === 'string') add(pr);
        for (const a of r.status?.result?.artifacts ?? []) {
          const url = safeURL(a.url ?? undefined);
          if (url) add(url, url);
        }
      }
      for (const e of detail.effects) {
        if (e.spec.task !== task.metadata.id) continue;
        const url = safeURL(e.status?.result?.url ?? undefined);
        if (url) add(url, url);
      }
      return {
        task,
        runs,
        latest,
        inFlight: IN_FLIGHT.has(task.status?.phase ?? 'Pending'),
        inputs,
        harness: harnessOf(task, latest),
        ...(result?.outcome ? { outcome: result.outcome } : {}),
        ...(result?.summary ? { summary: result.summary } : {}),
        produced,
      };
    });
}
