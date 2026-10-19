Release train 2026-11 over `repos.yaml`: **finished**.

Comment `/skip <repo>`, `/retry <repo>` or `/abort` on this issue.

| Stage | Repo | Status | From | Released | PRs | Detail |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | api | released | v1.3.2 | v1.4.0 | release [api#412](https://github.com/compliance-framework/api/pull/412) |  |
| 2 | agent | released | v0.9.1 | v0.9.2 | release [agent#234](https://github.com/compliance-framework/agent/pull/234) |  |
| 3 | helm-charts | released | agent v1.0.0, ccf v2.1.0 | agent v1.0.1, ccf v3.0.0 | release [helm-charts#98](https://github.com/compliance-framework/helm-charts/pull/98) |  |
| 3 | ui | skipped | v2.0.0 |  |  |  |

<!-- train-state
{"month":"2026-11","manifest":"repos.yaml","status":"finished","channel":"C07TRAIN01","thread_ts":"1793612345.000100","board":"9f2c1d","repos":[{"name":"api","stage":1,"phase":"released","from":{".":"1.3.2"},"versions":{".":"1.4.0"},"tags":["v1.4.0"],"release_pr":412,"merge_sha":"a1b2c3d4"},{"name":"agent","stage":2,"phase":"released","from":{".":"0.9.1"},"versions":{".":"0.9.2"},"tags":["v0.9.2"],"bump_pr":233,"release_pr":234},{"name":"helm-charts","stage":3,"phase":"released","from":{"charts/ccf":"2.1.0","charts/agent":"1.0.0"},"versions":{"charts/ccf":"3.0.0","charts/agent":"1.0.1"},"tags":["ccf-v3.0.0","agent-v1.0.1"],"release_pr":98},{"name":"ui","stage":3,"phase":"skipped","detail":"skipped by admin","from":{".":"2.0.0"}}],"notified":["api|released||","agent|released||"],"last_comment":3412345678,"digest":"https://github.com/compliance-framework/workflows/issues/123#issuecomment-3412345679"}
-->
