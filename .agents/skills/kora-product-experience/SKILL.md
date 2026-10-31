---
name: kora-product-experience
description: Shape Kigae Cloud's product experience from customer problem and public product website through template discovery, onboarding, activation, and expansion. Use for product strategy, journey and information architecture, flow planning, and validating these paths; use kora-design-review for a focused UI critique.
---

# Kigae Product Experience

Use this skill when defining or improving how a prospective or existing Kigae
customer discovers a product capability, chooses a template or module,
configures a workspace, reaches a useful first outcome, or expands usage.
It supports product discovery, positioning, journey design, onboarding and
activation planning, and lifecycle decisions. It does not prescribe a visual
style.

## Read the product context

For work on the public website, template marketplace, onboarding, or expansion,
read:

- [Kigae product website and onboarding research library](../../../docs/design/kora-product-website-onboarding-research-library.md)
- [Kigae website review field guide](../../../docs/design/kora-website-review-field-guide.md)
- [Kigae website-to-Cloud product flow blueprint](../../../docs/design/kora-website-cloud-flow-blueprint.md)
- [Kigae Marketplace end-to-end operating plan](../../../docs/design/kora-marketplace-end-to-end-operating-plan.md)
- [Kigae Marketplace implementation guide](../../../docs/design/kora-marketplace-implementation-guide.md)
- [Template marketplace implementation plan](/home/asena/projects/kora-cloud/docs/template-marketplace-implementation-plan.md)

Inspect the relevant product surface, implementation, and available customer
evidence. Treat existing plans and proposed journey steps as current direction,
not proof that customers need or understand them. If the work is a detailed
interface critique or visual refinement, also use the
[Kigae design-review skill](../kora-design-review/SKILL.md).

## Product workflow

### Establish the customer and outcome

Clarify the customer segment, user and buyer roles, the job they are trying to
complete, the situation that triggers it, and what success looks like in their
terms. Prefer observed or recent behavior, existing workarounds, and real
constraints over opinions about hypothetical features. Keep assumptions
explicit and identify which decision depends on each one.

For research plans, include people who completed, abandoned, or needed help
with comparable work. Consider the operating context Kigae serves: mobile use,
connectivity, payments, data migration, language, implementation support, and
local business practices. Do not assume every customer is self-serve or that a
single onboarding route fits all segments.

### Design the whole path

Map the experience from discovery through evaluation, setup, first meaningful
task, support, repeat use, and expansion. Include the service work behind the
interface: integrations, data readiness, permissions, billing, and human
handoffs.

For public discovery, organize around customer goals and questions. Make the
audience, business outcome, product behavior, proof, requirements, and next
step understandable. For templates and modules, explain inclusions, optional
choices, dependencies, compatibility, integration behavior, and price or plan
impact in the language needed to make a decision.

For onboarding, preserve the user's discovery choices. Use good defaults for
safe and reversible decisions, explain consequences before consequential
choices, and show clear progress and feedback for validation, loading,
completion, and recovery. Reveal complexity in context. Define activation as a
customer outcome, such as completing a meaningful workflow task, rather than
an account-created or wizard-finished event.

For existing customers, make an upgrade or cross-sell relevant to their current
workflow. State the job it enables, compatibility and dependencies, cost or
plan impact, and installation effect. Do not recommend an addition solely
because it is available in the catalog.

### Reduce uncertainty with evidence

Choose the lightest research method that can answer the product decision:
customer interviews for context and past behavior, journey mapping for
cross-stage gaps, prototype testing for comprehension and task flow, and
analytics for locating live drop-offs. Funnels show where to investigate, not
why a customer struggled.

Test realistic tasks without explaining the interface first. Observe where
people hesitate, what they expect, what information is missing, and how they
recover from errors. Cover key roles, mobile use, keyboard access, accessibility
needs, and realistic network conditions. Treat qualitative sample guidance as
a starting point for iterative learning, not statistical proof.

After release, connect behavioral measures to the intended customer outcome:
catalog discovery, comparison, onboarding start and completion, setup errors,
time to first meaningful task, help requests, repeat use, and expansion where
appropriate. Pair metrics with continued customer conversations and update
the product hypothesis when evidence disagrees.

## Expected work products

Choose the smallest artifact that makes the decision actionable. Depending on
the request, produce:

- a concise problem framing with target customer, outcome, evidence, and
  assumptions;
- a journey or product flow with user needs, decision points, dependencies,
  failure paths, and service handoffs;
- an information architecture or page-content model for public discovery and
  catalog detail;
- an onboarding and activation proposal tied to a specific first customer
  outcome;
- a research plan, interview prompts, prototype tasks, or measurement plan;
- prioritized recommendations with evidence, customer impact, and next steps.

Keep proposed paths labeled as hypotheses until validated. Separate what is
known from what is inferred and what still needs research. Link product
recommendations to implementation constraints in the Cloud plan without
letting internal package terminology become customer-facing language by
default.

## Sources and limits

Use the linked research library for books, NN/g videos, GOV.UK methods, and the
Kigae-specific question set. Use the field guide for the existing YC design
review collection and transcript summaries. Treat expert reviews and
commercial product-led-growth frameworks as prompts and heuristics. Validate
recommendations with Kigae customers, product behavior, and operational evidence.
