# PLAN

Read repository guidance (AGENTS.md), architecture entry points, relevant symbols/tests, issue and PR metadata, and direct prerequisites only. Identify behavior already present with code/test evidence. Classify the request as epic, feature, executable task, or acceptance/integration task.

Split only independent outcomes. Each slice should be independently mergeable and observable, normally affecting about 1–5 meaningful implementation files. Keep cohesive work together if splitting breaks the repo or creates artificial interfaces. Include actual dependency edges and integration order. Do not infer dependency absence when relationship metadata is unavailable.

For every proposed slice, use the issue template. Give each criterion a stable AC-N identifier and an exact native project command or inspection procedure. State known risks and exclusions. Keep evidence blank until observed during execution. Report existing work reused, ordered slices, verification, and approval needed. Do not mutate GitHub or code until the user approves the draft.

After approval, create issues with `gh issue create`, link parent and blockers using supported GitHub relationships (for example the repository's issue dependency API/CLI capability). Verify each created relationship by querying it. Text references alone are not blockers. If the installed GitHub tooling cannot create actual blockers, state this and stop before claiming dependency setup succeeded.
