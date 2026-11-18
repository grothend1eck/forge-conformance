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
| Protected automerge | The server records who scheduled the merge. Success on the wrong SHA or in the wrong status context cannot release it. Passing the required check on the PR head merges the original request without another CLI run. Manual and WIP requests stay open. Git ancestry distinguishes an explicit merge from the repository's default squash. |
| Git SCM | Native Git verifies the private checkout, shallow/single-branch reads, direct and working-branch commits, `--existing-only`, and squash across two updates. A `gittag` target must create its annotated tag on the selected commit and leave it alone on reapply. |
| Branches, tags and releases | Select real refs, feed a condition or target from the result, and distinguish absence from a held-back release. A release on page two must be selectable; ensuring it exists must keep its original ID, contents and Git refs. |
| Action discovery | Resolve fully qualified actions in Gitea/Forgejo workflow and composite-action steps. Check release, tag and branch inputs, pins and version comments, and untouched neighbors. This tests Updatecli's crawler, not a forge Actions runner. |

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

## Known failures

With a `gitbranch` target and `scmid` pointing to `gitea`, an existing tag
with the requested branch name can make `apply` exit zero **without creating
the branch**. The [branch test](known_test.go) checks the remote branch and tag.

Run the reproducer on Gitea and Forgejo with `UPDATECLI_BIN` set:

```sh
go test -tags=acceptance,knownissues -count=1 -run '^TestNativeGitBranchPush$' ./acceptance/forge
```

Move the test into its capability when the fix lands. It does not count as passing.

## Scope

`gitea` is the SCM, `gitea/pullrequest` is the action, and `gitea/branch`,
`gitea/tag` and `gitea/release` are the resources. Forgejo uses that same
provider. Branch/tag resource targets explicitly reject writes. Release
changelogs, PR labels, cross-fork publishing and a `giteasearch` SCM are not
implemented at this baseline.

The [existing Codeberg recipes](../../e2e/updatecli.d/success.d/gitea) still
cover public hosting and bare-host HTTPS URLs. Local HTTP servers do not
qualify either. See [the Gitea/Forgejo fixture](fixture_test.go) and
[the GitLab fixture](gitlab_fixture_test.go) for pinned versions. Passing the
GitLab publication case does not qualify GitLab auto-merge or hosted GitLab.com.
