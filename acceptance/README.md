# Acceptance tests

These tests run a built Updatecli binary. The [family overview](CAPABILITIES.md)
lists the behaviors we want to compare across plugins. To see the capabilities
enrolled today, including gaps and reasons for N/A:

```sh
go run ./acceptance/cmd/conformance
```

## Run a forge

```sh
go build -o bin/updatecli .
export UPDATECLI_BIN="$PWD/bin/updatecli"
UPDATECLI_TEST_FORGE=forgejo go test -tags=acceptance -count=1 -v ./acceptance/forge/gitea
```

Use `gitea` for Gitea, or run `./acceptance/forge/gitlab` for GitLab CE. Each
profile starts one disposable container. Git and the forge API seed private
repositories and inspect what the candidate actually published. You need
Docker and Git locally. The fixture supplies its own credentials.

Use Go's `-run` to work on one regression, for example
`-run '^TestCapabilities$/^forgejo$/^request_safety$/^draft_survives_edit$'`.
The `knownissues` build tag also exposes `TestPrivateNativeRefTargets`, a
reproducer for private authenticated branch and tag creation that's still red.

## Add a case

- **Another implementation:** wire its fixture to an existing contract. Both
  the Gitea PR and GitLab MR drive the same `lifecycle` and `already_current`
  tests in [publication.go](forge/publication.go).
- **A feature or regression:** add a `t.Run` beside the behavior it protects.
  When adding a whole capability, enroll it in the family's
  [matrix](forge/matrix.go) and run it from the provider's `TestCapabilities`.
  Individual regressions do not need a new row or a workflow edit.
- **A fuzz target:** keep the `Fuzz…` function in the package that owns the
  behavior. The [draft-title target](../pkg/plugins/resources/gitea/pullrequest/title_fuzz_test.go)
  checks repeat edits; the real-forge regression checks that an edited draft
  stays open. No registration is needed: PRs replay the seeds, and the nightly
  matrix gives each discovered target a minute. To see the same list locally,
  run `GOFLAGS=-tags=acceptance go run ./acceptance/cmd/fuzzmatrix`.
- **Another family:** use [`internal/cli`](internal/cli) to drive the binary
  and `testing.T` for the assertions. If it compares implementations, give
  it a `conformance.Matrix` and register that matrix in `cmd/conformance`.
  File plugins don't need a forge fixture.

## What CI reports

Every PR runs the complete matrix. Jobs share one native GoReleaser snapshot.
The summary reads Go's own test events. A required group only counts if the
whole provider run passed; a failed, skipped or missing case doesn't turn
green. Only N/A reduces the applicable count. To reproduce a summary, put
one unfiltered `go test -json -count=1` stream per profile in
`results/forge/{gitea,forgejo,gitlab}.jsonl`, then run
`go run ./acceptance/cmd/conformance results`.
