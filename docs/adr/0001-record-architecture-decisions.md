# ADR-0001: Record architecture decisions

**Status:** Accepted
**Date:** 2026-07-09

## Context

This platform will be built across several milestones by people who were not present for this one. The architecture documents describe *what* the system is. They are poorly suited to explaining *why* an option that looks obviously better was rejected — that reasoning lives in a designer's head and evaporates.

The predictable failure: eighteen months from now, someone sees `capacity-optimized-prioritized` Spot allocation, reasons that `lowest-price` would be cheaper, changes it, and rediscovers Spot churn the expensive way. The architecture document said what we do. It did not say what happens if you do the other thing.

## Decision

Record every load-bearing architectural decision as an ADR in `docs/adr/`, using Nygard's format extended with an explicit **negative consequences** section.

An ADR is **immutable** once accepted. Changing a decision means writing a new ADR that supersedes the old one. The superseded ADR stays, so the reasoning trail is intact.

A decision is load-bearing — and therefore needs an ADR — if it satisfies any of:

- Reversing it later would cost more than a sprint.
- It rejects an option a competent engineer would reasonably choose.
- It trades one desirable property for another (cost for latency, simplicity for flexibility).
- It commits the platform to a technology, a boundary, or a data model.

Everything else is a decision, not an architecture decision, and belongs in the code.

## Consequences

**Positive**

- Rationale survives staff turnover; the *rejected* alternatives survive with it, which is the scarcer half.
- Onboarding reads as a narrative rather than an archaeology dig.
- Reversal is cheap to evaluate: the ADR already lists what changing it costs.
- Writing an ADR is a forcing function. A decision that cannot be justified in a page usually has not been made yet.

**Negative**

- Ongoing discipline. ADRs that stop being written are worse than none, because the gaps look like decisions nobody made rather than decisions nobody recorded.
- Judgement required on what qualifies. Too many and the set is unreadable; too few and the important ones are lost among the trivial.
- Immutability feels wasteful when a decision changes twice in a month.

## Alternatives considered

**Rationale inline in architecture docs.** Rejected: rationale and description have different lifecycles. The description is updated when the system changes; the rationale must not be, because it explains a decision made under conditions that no longer hold. Mixing them means either the rationale rots or the description does.

**A decision log in the wiki.** Rejected: not versioned with the code, not reviewed with the code, not present in the repository a future engineer clones.

**Nothing.** Rejected. This is how `lowest-price` gets set.
