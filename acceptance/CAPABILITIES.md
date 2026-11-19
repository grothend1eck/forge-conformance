## Updatecli capability families

Find a test by the behavior a user expects. For example, `yaml` belongs to
reading, conditions, and writing; those are three different promises. A
family compares implementations of the same promise. A single pipeline
may exercise several families without qualifying every one of them.

| Family | Shared behavior to verify | Example implementations or inputs | Family matrix |
| --- | --- | --- | --- |
| Reading values | Read the selected value; distinguish a missing value from an empty one. | File, YAML, JSON, XML, TOML, HCL, CSV, HTTP | Not enrolled |
| Writing files | Preview without writing, edit only the selected value, reapply without a change. | YAML, JSON, TOML, XML, Dockerfile, `.tool-versions`, Terraform lock files | YAML smoke test; YAML/JSON matrix next |
| Resolving versions and digests | Choose the eligible release, tag or digest. Check age only for sources with meaningful dates. | Helm, Docker images, npm, PyPI, Maven, Cargo, Go modules, cloud images | Not enrolled |
| Changelogs | Collect the notes between the previous and proposed version when the source supplies them. | GitHub releases, npm, Go modules, Helm charts | No separate matrix |
| Conditions | Distinguish a match from a mismatch or lookup error; a target must not write when its prerequisite is unmet. | File selectors, Git refs, releases, OSV | Not enrolled |
| Transforming values | Apply ordered transformations to source data and pass the result to the consumer. | Replacements, regexp captures, semver increment, JSON selection | Not enrolled |
| SCM and repository search | Check out the right ref, publish only the intended change, or expand a repository query to the expected set. | Plain Git, forge SCMs, GitHub/GitLab/Azure DevOps search | SCM cases in forge suite; no separate matrix |
| Forges | Publish, update and clean up requests; honor reviewers, protected merges and release identity. | Gitea, Forgejo, GitLab | [Forge matrix](forge/matrix.go) |
| Discovery and recipes | Discover the intended dependency, honor exclusions and apply the generated manifest to the right file. | Go, npm, Helm, Terraform, Kubernetes, Actions and other crawlers | Actions cases in forge suite; no separate matrix |
| Command execution | Pass the expected environment and dry-run flag; distinguish no change, a change and a failing exit. | Shell sources, conditions and targets; tool-backed recipes | Not enrolled |
| Manifests, composition and CLI | Load values/templates, resolve relative paths, validate inputs and honor the candidate's minimum version. | Manifest files, compose stacks, scaffolded policies and CLI commands | Candidate smoke tests; no family matrix |
| Pipeline execution | Order dependent resources, route source values, and stop or skip downstream work for the right reason. | Source → condition → target → action; multiple manifests | Not enrolled |
| Policy distribution | Push a policy, resolve and pull the intended artifact, and preserve its assets and values. | OCI registries, policy bundles, tag and digest references | Not enrolled |
| Reporting and diagnostics | Report what changed without exposing credentials; preserve request/report identity across updates. | Console and exported reports, CI run links, `udash`, telemetry | PR report cases in forge suite; no separate matrix |

“Not enrolled” means this reporting system has no family matrix yet. The
existing package and end-to-end tests still count as tests; their results
are not silently converted into compliance scores. Each measured family
owns its applicable capabilities, reasons for N/A, and required profiles.
There is no project-wide percentage that adds unrelated denominators.

For example, a writing contract can give `controller.version` and
`audit.version` the same value and edit only the controller. `diff` leaves
the bytes alone; `apply` changes one field; reapplying leaves the file alone.
The same case works for YAML and JSON.
