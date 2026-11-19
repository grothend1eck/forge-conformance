# Forge acceptance tests

`Forge acceptance` builds one native Updatecli snapshot and passes it to
three parallel jobs: Gitea, Forgejo and GitLab. Each job starts a disposable
server and gives the candidate a token issued to a non-admin user there.
Fixture repositories start private.

From the repository root, with Go, Git and Docker:

```sh
mkdir -p bin
go build -o bin/updatecli .
export UPDATECLI_BIN="$PWD/bin/updatecli"
UPDATECLI_TEST_FORGE=forgejo go test -tags=acceptance -count=1 -v ./acceptance/forge
```

Use `gitea`, `forgejo` or `gitlab`; leave `UPDATECLI_TEST_FORGE` unset to run
all three. GitLab pulls the full CE image on a cold run. To focus on one case:
`-run '^TestForge$/^forgejo$/^server_automerge$/^observably_scheduled$'`.

## Which capabilities run?

[The `profiles` table](forge_test.go) is the enrollment list. Gitea and Forgejo
use the same `gitea` provider and run all the groups below. GitLab uses the
`gitlab` provider and currently runs **`publish_update` only**.

| Contract | What we observe |
| --- | --- |
| Authentication | A token alone can read the non-default branch of a private repo. Anonymous Git access fails until the repo is made public; the public branch is also readable over REST. |
| Publish and edit a PR/MR | `lifecycle`: preview without writes, publish on the non-default base, reapply without changes, then change the file and title on the original request. `already_current`: if the configured base already has the requested version, no branch or request is created. |
| PR review metadata (Gitea and Forgejo) | An update keeps the assignee and completed approval on the same PR and requests only the new reviewer. |
| Close and isolate requests | Keep nonempty PRs; identify and close an empty managed PR, and prune only its working branch. A same-named PR **from another fork** is not Updatecli's PR. |
| Review safety | An edit keeps a maintainer-marked draft on the same PR. If the forge rejects a reviewer, Updatecli reports failure and does not schedule a merge, even after the required check passes. |
| Protected automerge | The server records who scheduled the merge. Success on the wrong SHA or in the wrong status context cannot release it. Passing the required check on the PR head merges the original request without another CLI run. An edit keeps an existing schedule. Native Git distinguishes explicit merge and fast-forward from the repository's default squash. |
| Git SCM | Native Git verifies private shallow and submodule checkouts, commit contents, signatures, direct and working-branch updates, squash and pruning. Native branch and tag targets leave same-named sibling refs alone. |
| Branches, tags and releases | Select real refs, feed a condition or target from the result, and distinguish absence from a held-back release. A release on page two must be selectable; ensuring it exists must keep its original ID, contents and Git refs. |
| Action discovery | Resolve fully qualified actions in Gitea/Forgejo workflow and composite-action steps. Check release, tag and branch inputs, pins and version comments, and untouched neighbors. This tests Updatecli's crawler, not a forge Actions runner. |
| Gitea Actions run link | With the documented runner variables supplied, the generated PR body links to the run **number**, not the run ID. No Actions worker is needed. |

## Adding a capability

Add a regression where its behavior is defined. For example, the shared
[`publish_update` cases](publication_test.go) run on every enrolled forge;
an assertion about Gitea reviewers stays with the Gitea provider.
For a new capability, add its test function to the profile's list.
An enrolled case that calls `t.Skip` fails. The workflow runs the whole selected
profile and reports failures under their Go subtest names.

Drive the operation through Updatecli. Use Git or the forge API to set up
the repository and verify what changed.

The real `draft_survives_edit` case verifies that a maintainer's draft stays
open. Its [package fuzz test](../../pkg/plugins/resources/gitea/pullrequest/title_fuzz_test.go)
checks repeated title edits without a server. Ordinary `go test` replays the
seeds; the nightly job explores new inputs for one minute. To run it locally:

```sh
go test -run '^$' -fuzz '^FuzzExistingPullRequestTitle$' -fuzztime=1m ./pkg/plugins/resources/gitea/pullrequest
```

## Reading the conformance check

`Forge conformance` compares capability groups, not subtest counts. Each
matrix job uploads a JSON report from its real test run. Only a passed group
from a successful, complete run counts as verified. Missing evidence fails
the full check. **Only N/A is excluded from the denominator.** For example,
GitLab does not use the Gitea Actions run-number URL. A missing Updatecli
feature, an untested group, and a known bug stay visible. This is the
catalog in [`forge_test.go`](forge_test.go), not a percentage of a forge's API.

For local evidence, set `UPDATECLI_TEST_FORGE` and `UPDATECLI_TEST_REPORT`
to one profile and a new absolute JSON filename. Run its whole suite with
`-count=1`. Use `-run` without a report for day-to-day work.

**Known issue:** the generic native ref targets can attempt an anonymous
checkout of a private repository before the authenticated push. The
[reproducer](known_private_refs_test.go) checks the branch and the managed
tag case on both servers. Run it with `UPDATECLI_BIN` set:

```sh
go test -tags=acceptance,knownissues -count=1 -run '^TestPrivateNativeRefTargets$' ./acceptance/forge
```

The ordinary public branch/tag case and private direct-base tag control are
separate. A green result for either does not clear this private checkout bug.

## Scope

`gitea` is the SCM, `gitea/pullrequest` is the action, and `gitea/branch`,
`gitea/tag` and `gitea/release` are the resources. Forgejo uses that same
provider. Branch/tag resource targets explicitly reject writes. Release
changelogs, PR labels, cross-fork publishing and a `giteasearch` SCM are not
implemented at this baseline.

The [existing Codeberg recipes](../../e2e/updatecli.d/success.d/gitea) still
cover public hosting and bare-host HTTPS URLs. Local HTTP servers do not
qualify either. See [the Gitea/Forgejo fixture](gitea_fixture_test.go) and
[the GitLab fixture](gitlab_fixture_test.go) for pinned versions. Passing the
GitLab publication case does not qualify GitLab auto-merge or hosted GitLab.com.

On PRs that touch only the two acceptance READMEs, the forge check reports
“not run.” An edit confined to the draft-title fuzz target or its corpus
runs the full Go pull-request package (including the seeds). Source changes,
other tests, fixtures, module or workflow changes, deletions, renames and
ambiguous history all run the full matrix. A manual dispatch can always
request a full run.
