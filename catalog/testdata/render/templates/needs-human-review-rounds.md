🙋 The factory needs a person here.

- **What blocked:** the `docs-review` run ended `changes-requested`: Third round: CCF_BATCH_SIZE's default is still wrong.
- **Last run:** `run_rev3`, in the [Factory UI](https://factory.example/workitems/wi_needs-human-review-rounds)
- **Escalated by:** the `escalate-needs-human` rule, which also added the `needs-human` label.
- **To resume:** fix what blocked, then push a commit (a new commit starts a new review), or ask a maintainer to redispatch the last task from the Factory UI. Remove `needs-human` once it's handled. To stop the factory here, add `factory:stop`.
