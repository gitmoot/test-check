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
