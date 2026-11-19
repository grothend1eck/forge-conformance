## Acceptance coverage

Only the forge family has a support matrix. A dash means no dedicated
acceptance suite yet; existing unit and end-to-end tests are outside this table.

| Family | Examples | Acceptance tests |
| --- | --- | --- |
| Reading values | File, YAML, JSON, HTTP | — |
| Writing files | YAML, JSON, Dockerfile | Local YAML `diff` and `apply` |
| Versions and digests | Helm, Docker, npm, Go modules | — |
| Changelogs | Release notes between versions | — |
| Conditions | File values, refs, releases, OSV | — |
| Transformations | Regexp, semver, JSON selection | — |
| SCM and repository search | Checkout, push, repository queries | Git operations in the forge suite |
| Forges | Requests, reviews, merges, releases | [Forge matrix](forge/matrix.go) |
| Discovery and recipes | Actions, Helm, Terraform | Actions `uses:` discovery in the forge suite |
| Command execution | Shell sources, conditions, targets | Fixture executable and environment |
| Manifests, composition and CLI | Values, templates, minimum version | Candidate smoke tests |
| Pipeline execution | Source → condition → target → action | — |
| Policy distribution | OCI push and pull | — |
| Reporting and diagnostics | Request reports, CI run links | Forge reports and run links |

**Supported** requires a passing test. A **gap** is possible on the provider
but missing in Updatecli. **Not applicable** means there is no equivalent
operation. An omitted cell is **unassessed**.

The report counts supported cells and gaps as *assessed applicable*. It counts
a profile's supported cells as *verified* only after a complete, passing run.
