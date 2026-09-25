---
name: test-check
description: Decides what testing a code change needs — a regression test that goes red on the old code, a case in an existing test, a one-off check, or none. Use before writing or finalizing tests for a change, and when judging tests a change adds.
---

A permanent test earns its place only if it protects behavior someone relies on and goes **red** on a plausible regression: it fails on the old code and passes on the new. A test that cannot go red is cost without protection.

## Steps

1. **Ask.** In the checkout, run `test-check` (it reads everything since the merge base, including uncommitted files). For a pushed PR: `test-check --repo OWNER/REPO --pr N`. Done when it prints `advice:`, or exits 3 (JEV unavailable) — then answer the four questions below yourself and act on your answers.
2. **Act on `advice`:**

   | advice | do |
   |---|---|
   | `new_regression_test` | Write one test for the changed behavior. |
   | `extend_existing_test` | Add a case to the named nearby test instead of a new test. |
   | `one_off_check` | No permanent test. Run the changed path once. |
   | `no_permanent_test` | No new test. Build and run the existing tests. |
   | `need_context` | Answer the four questions yourself. |

   If it warns that an added test looks **low-value**, rework it until it can go red, or delete it.
3. **Prove it.** For a new or extended test: run it on the old code (stash or revert the fix) and see it go red, then on the new code and see it go green. For a one-off check: run it and keep the output. Done when the PR description records the red and green runs (or the one-off command and what you observed) — not before.

`test-check` is advice. Your own evidence decides; never skip verification because it said no test.

## The four questions

1. What behavior does the test protect, for whom?
2. What plausible bug would make it go red?
3. Why do the existing tests not catch that bug already?
4. Does it force test-only code, hooks, or exports into the product? If yes, test through the real interface instead.

A test that restates source text or constants, copies fixtures, asserts private internals or call order, or duplicates a stronger test fails question 2 or 3: do not add it.
