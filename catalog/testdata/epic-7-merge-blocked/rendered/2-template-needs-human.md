🙋 The factory needs a person here.

- **What blocked:** the `merge` run ended `blocked`
- **Last run:** `run-merge-2`, in the [Factory UI](https://factory.example/workitems/wi-epic)
- **Escalated by:** the `escalate-needs-human` rule, which also added the `needs-human` label.
- **To resume:** fix what blocked, then push a commit (a new commit starts a new review), or ask a maintainer to redispatch the last task from the Factory UI. Remove `needs-human` once it's handled. To stop the factory here, add `factory:stop`.
