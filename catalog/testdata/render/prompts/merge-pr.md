# Task: merge for compliance-framework/docs

## Goal

Re-verify the merge gates and merge the work item's pull request.

## Inputs

- target: "pr"
- requiredCheck: "build"
- merge label: `ready-to-merge`
- repo: `compliance-framework/docs`
- work item: `wi_merge-pr`
- pull request: `compliance-framework/docs#50`

## Use these skills

- **merge** (ccf-factory) for the procedure. Follow its "Headless mode (CCF Factory)" section.

## Rules

- Gates, checked on each PR right before merging it:
  - approved by the factory's reviewer at the PR's head commit;
  - the required check "build" succeeded at that commit;
  - no unresolved review thread;
  - no hold label: `do-not-merge`, `factory:hold` or `factory:stop`;
  - no human `CHANGES_REQUESTED` review newer than the factory's approval.
- Only those explicit signals count; never read intent from comment wording.
- A failed gate ends the run `blocked`, with the PR and the gate in `data.blockedBy`.
- One pass: never wait, sleep or poll for a check or a review. The rules dispatch merge again
  when the check completes or the approval lands.
- When review requirements block the merge because the reviewer App's approval doesn't count,
  add the `ready-to-merge` label instead (the merge op's merge label): outcome
  `labelled-for-merge`. That label is for a person to merge; it is the only label you add.
- Never force-merge, bypass rulesets, or merge a PR the factory didn't author.

## Done when

- [ ] Every PR is merged (outcome `merged`), labelled for a person to merge, or the run says
      which PR and gate stopped it (`blocked`) or which PR it waits on (`pending`).
- [ ] The Result is written and `factory result --file "$FACTORY_OUT/result.json"` accepts it.

## Result

Write JSON to `$FACTORY_RESULT` (with `factory result`) matching the `merge` schema; outcome is
one of: merged, labelled-for-merge, rebased-with-conflicts, pending, blocked, needs-human.
`data`: `merged`, one `{pr: "owner/repo#N", sha}` per PR merged, in merge order; `blockedBy:
{pr, reason}` when a gate or a rebase stopped the run; and `waitingOn: {pr, reason}` when the
run ends `pending` (the next PR's required check is pending, or its approval is at an old head).
