# Gitea and Forgejo acceptance tests

`Forge acceptance` builds the Updatecli executable once and exercises the `gitea`
plugin on two real servers. Gitea and Forgejo are separate matrix jobs. Each
boots one disposable server and creates a fresh private repository for each
scenario. The candidate gets a token issued by that server. No Codeberg or
GitHub account is needed to run the tests.

From the repository root, with Go, Git and Docker:

```sh
mkdir -p bin
go build -o bin/updatecli .
export UPDATECLI_BIN="$PWD/bin/updatecli"
UPDATECLI_TEST_FORGE=forgejo go test -tags=acceptance -count=1 -v ./acceptance/forge
```

Select `gitea` for Gitea, or omit `UPDATECLI_TEST_FORGE` to run both. Use Go's
`-run` flag to troubleshoot one case, for example
`-run '^TestForge$/^forgejo$/^server_automerge$/^observably_scheduled$'`.
The public check uses the entire inventory in [cases.txt](cases.txt). A skip,
missing case, test failure or incomplete test stream is a failed check.

| Contract | What we observe |
| --- | --- |
| Publish and edit a PR | `diff` leaves remote refs alone. `apply` opens a single PR on the non-default base with the intended file. A no-op preserves the head; a newer input edits the same request. The API exposes its title, body, assignee and requested reviews. |
| Close and isolate requests | Keep nonempty PRs; identify and close an empty managed PR, and prune only its working branch. A same-named PR **from another fork** is not Updatecli's PR. |
| Protected automerge | The server records a native scheduling event. Wrong SHA or status context cannot advance the base. Completing the required check on the managed head merges that PR **without another CLI run**. Manual and WIP requests stay open. |
| Git SCM | Read private files from the configured branch; shallow/single-branch reads, a direct fast-forward commit, author/message, custom working refs and `--existing-only` are checked with native Git. |
| Branches, tags and releases | Select real refs, feed a condition or target from the result, distinguish absence from a held-back release, and verify effects on local files. The release target previews, creates a published or draft release and leaves an existing release alone on reapply. |
| Action discovery | Resolve a fully qualified action on the fixture server. Check release, tag and branch inputs, version and digest modes, and which files were edited in `.gitea/workflows` and `.forgejo/workflows`. This tests Updatecli's crawler, not a forge Actions runner. |

## Adding a capability

Add the shared scenario to `forge_test.go` and the expected leaf names to
`cases.txt`. A new case then runs for both servers without another workflow
job. Only seeding and observations should use the SDK. The operation being
tested must go through the built CLI. Prefer state the user can inspect: Git
refs and file contents for published edits; forge API state for a request,
review, status or release.

An empty Gitea release changelog, branch/tag **resource** targets, PR label
management and a native `giteasearch` SCM are not supported at the baseline
used to introduce this suite. The release target ensures existence, not metadata
reconciliation. Neither a fork-isolation case nor a preserved WIP title implies
support for publishing cross-fork requests or a `draft` manifest field.
Service version pins, test identities and readiness checks live in
`fixture_test.go`. A passing profile applies to that tested version, not every
Gitea or Forgejo deployment.
