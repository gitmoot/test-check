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
3. **Prove it.** For a new or extended test, run `test-check prove --test "<verbose command for ONE test, with {name}>"` — for example `go test ./pkg -v -run '^{name}$'` or `python3 -m pytest -v -k '{name}'`. It finds the tests your change adds or edits (Go and Python; otherwise pass `--each A,B`), runs each on your new code, then again with every non-test change reverted, and restores your files. **Every** test must go red on its own. Outcomes:

   | outcome | exit | meaning |
   |---|---|---|
   | `proven` | 0 | Red on the old code, green on the new. Done. |
   | `not_red_on_old` | 4 | The test passes without the fix. Make it fail without the fix — unless it is a guard test ("X is *not* matched", "Y stays unchanged") or only its fixture changed: those pass on old code by design; say so in the PR. |
   | `fails_on_new` | 4 | The test fails on your code. Fix that first. |
   | `no_test_in_change` | 4 | No test file changed. Add one, or treat it as a one-off check. |
   | `test_not_run` | 4 | The command never mentioned the test, so it probably selected nothing. Fix the command or the name. |

   A `build_error_suspected` warning means the old run failed only because the test uses new names; for a bug fix, test through an interface the old code already had. If a run is interrupted, `test-check prove --restore` puts your files back. For a one-off check: run it and keep the output.

   Optionally, look for changed code no test checks: `test-check prove --pieces --test "<command that runs the change's tests>"` (`test-check prove --list-tests` prints them). It undoes each changed piece of code on its own and lists every `not_tested` piece. Many are fine (logging, wording, defaults); use the list to decide what deserves a test, not as a pass/fail. Done when the PR description records the `prove` outcome (or the one-off command and what you observed) — not before.

`test-check` is advice. Your own evidence decides; never skip verification because it said no test.

## The four questions

1. What behavior does the test protect, for whom?
2. What plausible bug would make it go red?
3. Why do the existing tests not catch that bug already?
4. Does it force test-only code, hooks, or exports into the product? If yes, test through the real interface instead.

A test that restates source text or constants, copies fixtures, asserts private internals or call order, or duplicates a stronger test fails question 2 or 3: do not add it.
