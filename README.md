# test-check

Asks [JEV](https://openrouter.ai/typesafe) what testing a code change needs, so
agents write the regression tests that matter and skip the ones that only add
cost. Proposed in gitmoot/gitmoot#2265.

It sends JEV the diff and the nearest existing tests, and gets back one
**advice**:

| advice | meaning |
|---|---|
| `new_regression_test` | Behavior changed and nothing nearby tests it: add a test that fails on the old code. |
| `extend_existing_test` | A nearby test already covers this area: add a case to it. |
| `one_off_check` | Real change, but a permanent test would be brittle or only restate code: run it once. |
| `no_permanent_test` | No behavior change: build and run the existing tests. |
| `need_context` | JEV could not decide. |

It also reports two probabilities:
- **covered**: whether the change's own tests would fail on a regression.
- **low-value**: whether an added test only restates code, duplicates another test, tests internals, or would pass on the old code.

It is advice, not a gate. The agent still proves the change works; see the
[skill](skills/test-check/SKILL.md).

## Prove a test goes red

JEV reading a diff cannot tell whether a test would pass on the old code. So
`test-check prove` runs it:

```sh
test-check prove --test "go test ./pkg -v -run '^{name}$'"
```

1. It runs each test the change adds or edits on the working tree. Each must pass.
2. It puts every changed non-test file back to its base version and runs each test again. Each must fail.
3. It restores the files.

`{name}` is filled in for each test. The tests are taken from the change's diff for Go and Python; pass `--each A,B` for other runners. The command must print each test's name (use a verbose runner); otherwise the test is reported as `test_not_run`. Every test must go red on its own. A change is not proven just because one good test sits next to a weak one.

Without `{name}`, the command runs once for the whole change, and one red test is enough.

### Every changed piece must be tested

Red/green only checks the tests you wrote. `--pieces` checks the code you changed:

```sh
test-check prove --list-tests                       # the tests this change adds or edits
test-check prove --pieces --test "go test ./pkg -run '^(TestA|TestB)$'"
```

It undoes one changed piece of code at a time (one diff hunk, a whole added file, or a whole deleted file) and runs the command. The command must fail every time.
- A piece that leaves the command passing is reported `not_tested`, and the outcome is `piece_not_tested` (exit 4).
- Comment- and whitespace-only hunks are skipped.
- Above `--max-pieces` (default 60), nothing runs and the outcome is `too_many_pieces`.
- Each piece goes through the same save-first stash, so `--restore` recovers an interrupted run.

Test files and fixtures keep their new versions for both runs. The reverted files are saved under the git dir before anything changes. An interrupted or crashed run is recovered with `test-check prove --restore`, and a new run refuses to start until then.

Only one prove run may use a checkout at a time: a lock under the git dir makes a second run, or `--restore`, refuse while the first is alive. A lock left by a dead process is taken over. A test that hangs on the old code until `--timeout` counts as red.

Outcomes:
- `proven`: exit 0.
- `not_red_on_old`: exit 4. The test passes without the fix.
- `fails_on_new`: exit 4.
- `no_test_in_change`: exit 4.
- `test_not_run`: exit 4.
- `no_code_in_change`: exit 0. Only tests changed.

A `build_error_suspected` flag marks an old-code failure that looks like a compile or import error rather than a failed assertion.

## Use

```sh
test-check                                   # local checkout, incl. uncommitted files
test-check --repo OWNER/REPO --pr N          # a pull request's current head
test-check --repo OWNER/REPO --compare BASE...HEAD
test-check --json                            # machine-readable
```

Exit codes:
- `0`: advice printed.
- `1`: the change could not be read.
- `2`: usage error.
- `3`: JEV unavailable. No advice is printed; answer the skill's four questions yourself.

The GitHub modes use the `gh` CLI. The key comes from `$OPENROUTER_API_KEY`,
else the `OPENROUTER_API_KEY` line in `~/.config/gitmoot/keychain.env`. The
model is pinned to `typesafe/jev-1.13`.

## Install

```sh
go build -o ~/.local/bin/test-check ./cmd/test-check
ln -s "$PWD/skills/test-check" ~/.claude/skills/test-check
```
