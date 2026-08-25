# Source notes

This skill is an adaptation, not a copy. These notes record what was taken from
where, so that the borrowed vocabulary can be traced and the local decisions can
be told apart from the upstream ones.

## Sources consulted

| Source                                                                                                                                                                                                                                                                 | License                                                                             | Use made of it                                             |
|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------|------------------------------------------------------------|
| [`citypaul/.dotfiles` → `reduce-system-complexity`](https://github.com/citypaul/.dotfiles/tree/main/claude/.claude/skills/reduce-system-complexity), itself an attributed adaptation of [`mintuz/skills` → `reducer`](https://github.com/mintuz/skills) by Adam Bulmer | MIT                                                                                 | Concepts and gate structure; no text copied                |
| [`code-yeongyu/oh-my-openagent` → `refactor`](https://github.com/code-yeongyu/oh-my-openagent/tree/dev/packages/shared-skills/skills/refactor)                                                                                                                         | Sustainable Use License (GitHub reports `NOASSERTION`) — not an open source license | Phase ordering read as prior art only; no text copied      |
| [`code-yeongyu/oh-my-openagent` → `remove-ai-slops`](https://github.com/code-yeongyu/oh-my-openagent/tree/dev/packages/shared-skills/skills/remove-ai-slops)                                                                                                           | Sustainable Use License (`NOASSERTION`)                                             | Ladder ordering and the preserve-list idea; no text copied |

| [Fowler's refactoring catalog](https://refactoring.guru/refactoring/catalog), as published by refactoring.guru | proprietary content, freely readable | Technique and smell **names** only, as shared vocabulary. No text reproduced |

Because two of the three skill sources are not open source licensed, nothing was
reproduced from them. What was taken is structural: the order in which passes
are safe to apply, and the idea that each pass owes a list of what it must not
remove.

## Retained concepts

- The framing itself: "Conserve behavior. Minimize mechanism." It comes from
  `reducer`, by way of `reduce-system-complexity`.
- A separate **behavior gate** and **mechanism gate**, answering different
  questions and both required.
- **Same-scope, same-method before and after counting**, and the prohibition on
  exporting mechanism to callers, to configuration, or to operations rather than
  removing it.
- The distinction between a **transition** that relocates mechanism and a
  **terminal** reduction that removes it, with only the latter permitted to claim
  the reduction.
- Diagnosis as a legitimate outcome: an unverifiable equivalence claim is worse
  than an unfinished cleanup.
- A **deletion ladder** applied before any smell removal (delete, then reuse,
  then simplify), so that effort is not spent improving code that should not
  exist.
- **Per-pass preserve-lists**, on the grounds that a cleanup pass is at least as
  likely to remove something load-bearing as to remove something useless.
- A **test-coverage gate** before editing, borrowed from `refactor`'s phase 3.
- The **names** of the refactoring techniques and code smells, and the catalog's
  own organizing idea that a smell indexes onto the techniques that resolve it.
  `references/catalog.md` carries that index. Only the names are borrowed: each
  entry is written here in its Go form, and roughly a third of the catalog is
  excluded with its reason, because Go has no inheritance and no exceptions.

## Local adaptations

- Named `code-cleanup`, because the request in this repository is usually
  "clean this up" rather than "reduce complexity," and the skill has to be found
  by that word.
- Replaced generic complexity dimensions with the metrics
  `scripts/measure.sh` can actually count in Go, so both gates read the same
  numbers and the mechanism gate is a diff rather than a judgment.
- Anchored every pass in a measurement from this repository, and put the current
  backlog in `references/worklist.md` with the date it was taken, so a stale
  number is visible as stale.
- Added the *Never touch* table. Generated output, synced charts, inherited
  upstream code carrying someone else's copyright, and shipped CRD fields are
  each a path where an otherwise correct cleanup is wrong here.
- Added the atlas-lib modernization pass and split the shared-primitive move out
  into `extract-to-atlas-lib`, which this skill's passes 3 and 4 delegate to.
  That is this repository's central duplication rule and no upstream source had
  an equivalent.
- Dropped the parallel-agent and team-mode orchestration from `refactor`, and
  the multi-agent fan-out from `remove-ai-slops`. A cleanup here is bounded by
  what one reviewable diff can hold, which is the real constraint, and the
  ordering between passes matters more than the throughput.
- Replaced "oversized module" line thresholds with the file-as-a-concern test
  from the local `new-files` skill, which asks whether the file's opening comment
  needs an "and also" rather than counting its lines.
- Kept the commit-shape rule, pure moves separate from edits, because the
  behavior gate here is partly a human reading the diff.
