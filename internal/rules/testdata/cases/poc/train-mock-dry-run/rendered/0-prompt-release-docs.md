
# Task: release-docs for compliance-framework/mock-docs

## Goal
Document every release in the train, in per-repository versioned docs.

## Inputs
- depthPolicy
- train

## Use these skills
Use the docs-release skill for the procedure.

## Done when
- The result JSON is written.

## Result
Write JSON to $FACTORY_RESULT matching the schema; outcome is one of: pr-opened, no-docs-change, blocked.
