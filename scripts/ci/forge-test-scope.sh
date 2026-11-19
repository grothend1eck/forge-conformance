#!/usr/bin/env bash
# Classify the change actually tested by a pull_request: base tree to merge
# tree. Unknown history, empty diffs, deletions and renames get the full run.
set -euo pipefail

full() { echo full; exit 0; }
[[ $# == 3 ]] || full
cd -- "$(git rev-parse --show-toplevel)" || full
merge=$1 base=$2 head=$3
[[ $(git rev-parse --verify "$merge^1") == "$base" &&
   $(git rev-parse --verify "$merge^2") == "$head" ]] || full
diff=(git diff --no-ext-diff --no-renames --ignore-submodules=none "$base" "$merge")
"${diff[@]}" --quiet && full
"${diff[@]}" --quiet --diff-filter=am || full
# An added symlink or submodule is an addition, not a type change. The
# second field of --raw is the new Git mode, independent of the pathname.
modes=$("${diff[@]}" --raw | cut -d' ' -f2 | sort -u) || full
[[ $modes == 100644 ]] || full

docs=(':(top,exclude,literal)acceptance/README.md' ':(top,exclude,literal)acceptance/forge/README.md')
if "${diff[@]}" --quiet -- . "${docs[@]}"; then
  echo docs
elif "${diff[@]}" --quiet -- . "${docs[@]}" \
    ':(top,exclude,literal)pkg/plugins/resources/gitea/pullrequest/title_fuzz_test.go' \
    ':(top,exclude,glob)pkg/plugins/resources/gitea/pullrequest/testdata/fuzz/FuzzExistingPullRequestTitle/*'; then
  echo package
else
  full
fi
