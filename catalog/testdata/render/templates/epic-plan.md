🗺️ The factory planned this epic as 3 sub-issues, one pull request each, in this order:

1. compliance-framework/docs#71: Enable Docusaurus i18n with pt-BR
1. compliance-framework/docs#72: PT-BR glossary (after compliance-framework/docs#71)
1. compliance-framework/docs#73: Translate the general section (after compliance-framework/docs#72)

**Stacking order.** Each pull request is based on the previous one's branch, the first on `main`. When the last one is open, the factory reviews the whole stack, then merges it bottom-up.

**To stop.** Add the `factory:stop` label to this issue to stop the factory on this epic. Add `factory:hold` to a pull request to keep it from being merged until you remove the label.

[Follow it in the Factory UI](https://factory.example/workitems/wi_epic-plan)
