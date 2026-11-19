# Acceptance tests

## Run locally

From the repository root, with Docker and Git installed:

```sh
go build -o bin/updatecli .
export UPDATECLI_BIN="$PWD/bin/updatecli"
UPDATECLI_TEST_FORGE=forgejo go test -tags=acceptance -count=1 -v ./acceptance/forge/gitea
```

Use `gitea` for Gitea. GitLab CE has its own package, `./acceptance/forge/gitlab`.
Each profile starts a disposable server and creates its own credentials.
Use `-run '^TestCapabilities$/^forgejo$/^draft_title_marker$'` to select one case.
The `knownissues` tag enables `TestPrivateNativeRefTargets`, an open regression.

Gitea and Forgejo run on PRs, pushes to `main`, nightly, and on dispatch.
Branch, tag, release and SCM cases use seeded repositories on both servers.
The four public Codeberg recipes run separately, anonymously, on the nightly
and manual workflows. To run that read-only check locally:

```sh
go test -tags=acceptance,live -count=1 -v ./acceptance/forge/gitea/live
```

## Results

The [acceptance workflow](../.github/workflows/acceptance.yaml) runs every
profile against the same Updatecli snapshot. See the [family overview](CAPABILITIES.md).

```sh
go run ./acceptance/cmd/conformance          # declared support
go run ./acceptance/cmd/conformance results  # observed results
```

For the second command, save one unfiltered `go test -tags=acceptance -json -count=1`
stream per profile in `results/<family>/<profile>.jsonl`.

## Add a test

- **Regression:** add a `t.Run` under the capability it protects.
- **Capability:** add a row to the [matrix](forge/matrix.go) and register its
  check in `TestCapabilities`. A new provider can reuse a contract by passing
  its fixture to it; see [GitLab publication](forge/gitlab/forge_test.go).
- **Fuzz property:** write one `Fuzz…` and use `conformance.Bind` to select
  the supported implementations, as in the [title tests](forge/title_fuzz_test.go).
  The [fuzz workflow](../.github/workflows/fuzz.yaml) discovers targets,
  replays seeds on PRs, and fuzzes for one minute per target nightly or on dispatch.
- **Family:** use [`internal/cli`](internal/cli) to run the candidate. For
  cross-implementation checks, register the family's matrix in `cmd/conformance`
  and call `conformance.Observe(t)` at the start of `TestCapabilities`.
