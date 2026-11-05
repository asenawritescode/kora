---
name: Agent task
about: One ticket = one PR, sized for an agent (see Agent Rules in CLAUDE.md)
title: "<ticket-id>: <type>(<area>): <title>"
labels: ["blocked"]
---

**Goal:** <one sentence: what changes and why>

**Done when:** <observable outcome a test can prove>

**Depends on** (start only when merged):
- [ ] asenawritescode/kora#<N> (<ticket-id>)

Rules: see this repo's M1 tracker. Branch from `main`, ≤300 lines, tests, CI green.
