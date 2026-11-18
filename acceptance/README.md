# Acceptance tests

The tests run a built Updatecli binary through its public commands. To run
the shared runner's local YAML test, from the repository root:

```sh
mkdir -p bin
go build -o bin/updatecli .
UPDATECLI_BIN="$PWD/bin/updatecli" go test -tags=acceptance -count=1 ./acceptance/internal/cli
```

[`internal/cli`](internal/cli) gives each test a working directory and home.
Use `Run` for commands, file trees and compose runs. `Pipeline` is a shortcut
for one manifest. Pass fixture variables explicitly, including `PATH` if
the test supplies a tool of its own. The runner's proxy catches unexpected
HTTP(S) from clients that use it; it isn't a network sandbox. The
[local tests](internal/cli/runner_test.go) need no forge or hosted credentials.

The [forge suite](forge/README.md) adds disposable servers and checks their
state with Git and the forge API.
