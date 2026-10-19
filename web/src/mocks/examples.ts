// The contract's examples (../api/examples/<operationId>.json), keyed by operation ID. See
// design/workstreams/foundation/LLD/contracts.md §HTTP API for their format.
const files = import.meta.glob<unknown>('../../../api/examples/*.json', {
  eager: true,
  import: 'default',
});

export const examples: ReadonlyMap<string, unknown> = new Map(
  Object.entries(files).map(([file, example]) => [
    file.replace(/^.*\//, '').replace(/\.json$/, ''),
    example,
  ]),
);
