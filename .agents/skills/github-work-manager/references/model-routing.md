# Model and effort routing

Recommend only models and effort settings discoverable from supported Codex configuration or user-provided availability. Never infer installation from documentation. If switching is unsupported, state recommendation and proceed with current model when suitable.

- Routine triage/status: efficient available model, low.
- Small clear edit/test: efficient model, low or medium.
- Ordinary decomposition or implementation: capable model, medium.
- Hard integration debugging or complex multi-file change: stronger available model, medium/high.
- Security, distributed concurrency, architecture conflict, or high ambiguity: strongest suitable available model, high.

Escalate on observed repeated acceptance-test failure, unresolved technical ambiguity, material security/correctness risk, or repeated faulty changes—not line count. Choose the least costly configuration meeting evidence quality. Do not claim to change the active model or invoke Goal unless the environment supports it. Goals must bind exact issue, outcome, verification, repo/scope, constraints, iteration and stopping rules; never select follow-on issues. Implementation Goal ends PR_READY.
