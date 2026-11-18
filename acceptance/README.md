# Acceptance tests

The tests run a built Updatecli binary through its public commands. To run
the shared runner's local YAML test, from the repository root:

```sh
mkdir -p bin
go build -o bin/updatecli .
UPDATECLI_BIN="$PWD/bin/updatecli" go test -tags=acceptance -count=1 ./acceptance/internal/cli
```

[`internal/cli`](internal/cli) gives each test a working directory and home.
Use `Run` for a command or `Pipeline` for a manifest. Pass fixture environment
variables explicitly; a suite that supplies credentials must redact them
before logging command output. The [local test](internal/cli/runner_test.go)
needs no service or account.

The [forge suite](forge/README.md) adds disposable servers and checks their
state with Git and the forge API.
