# Task: merge for compliance-framework/docs

## Goal

Re-verify the merge gates and merge the work item's pull-request stack bottom-up, retargeting
and rebasing as you go.

## Inputs

- target: "stack"
- requiredCheck: "ci / required"
- merge label: `ready-to-merge`
- repo: `compliance-framework/docs`
- work item: `wi_merge-stack`
- pull request: `compliance-framework/docs#81`
- pull request: `compliance-framework/docs#82`

## Use these skills

- **merge** (ccf-factory) for the procedure. Follow its "Headless mode (CCF Factory)" section.

## Rules

- Gates, checked on each PR right before merging it:
  - approved by the factory's reviewer at the PR's head commit;
  - the required check "ci / required" succeeded at that commit;
  - no unresolved review thread;
  - no hold label: `do-not-merge`, `factory:hold` or `factory:stop`;
  - no human `CHANGES_REQUESTED` review newer than the factory's approval.
- Only those explicit signals count; never read intent from comment wording.
- A failed gate ends the run `blocked`, with the PR and the gate in `data.blockedBy`.
- Stack, bottom to top: merge the PR (`factory pr merge`), retarget the next one to `main`
  (`factory pr retarget`), rebase it (`factory stack rebase`), push (`factory stack push`),
  wait for its required check (at most until the task's time limit), then go on. A rebase
  with conflicts ends the run `rebased-with-conflicts`: the rules send it back to review.
- When review requirements block the merge because the reviewer App's approval doesn't count,
  add the `ready-to-merge` label instead (the merge op's merge label): outcome
  `labelled-for-merge`. That label is for a person to merge; it is the only label you add.
- Never force-merge, bypass rulesets, or merge a PR the factory didn't author.

## Done when

- [ ] Every PR is merged (outcome `merged`), labelled for a person to merge, or the run says
      which PR and gate stopped it.
- [ ] The Result is written and `factory result --file "$FACTORY_OUT/result.json"` accepts it.

## Result

Write JSON to `$FACTORY_RESULT` (with `factory result`) matching the `merge` schema; outcome is
one of: merged, labelled-for-merge, rebased-with-conflicts, blocked, needs-human. `data`:
`merged`, one `{pr: "owner/repo#N", sha}` per PR merged, in merge order, and `blockedBy:
{pr, reason}` when a gate or a rebase stopped the run.
