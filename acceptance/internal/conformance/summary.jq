# Read one report per profile from the same Actions run. A missing report
# cannot reduce the denominator or become a green cell.
def ids: [.capabilities[].id];
def labels: {
  pass: "pass", fail: "FAIL", not_exercised: "not run", not_enrolled: "untested",
  unimplemented: "not implemented", not_applicable: "N/A", known_issue: "known issue"
};
def row($cells): "| " + ($cells | join(" | ")) + " |";
def checked:
  if .outcome == "success" and .bootstrap == "pass" and .complete == true then .
  else .capabilities |= map(if .status == "pass" then .status = "not_exercised" else . end)
  end;

map(checked) | sort_by(.profile) as $reports
| ($reports | map(.profile)) as $profiles
| if $profiles == [] or ($profiles | unique) != $profiles or
     any($reports[]; .version != 1 or .profiles != $profiles or
         .candidate != $candidate or .revision != $revision)
  then error("missing, duplicate or mismatched profile reports") else . end
| ($reports[0] | ids) as $ids
| $floor[0] as $v1
| if ($floor | length) != 1 or ($v1 | type) != "object" or ($v1 | length) == 0 or
     (($v1 | keys) - $ids | length) != 0 or
     any($v1[]; type != "array" or (. - $profiles | length) != 0)
  then error("missing v1 capabilities or required profiles") else . end
| if $ids == [] or ($ids | unique) != $ids or any($reports[]; ids != $ids) or
     any($reports[].capabilities[]; (labels[.status] == null) or
         ((.status == "not_applicable" or .status == "unimplemented" or .status == "known_issue") and (.reason | length) == 0))
  then error("inconsistent capability catalog or disposition") else . end
| "## Forge conformance", "",
  row(["Capability"] + $profiles),
  row(["---"] + [$profiles[] | "---"]),
  row(["**Run**"] + [$reports[].outcome]),
  (range($ids | length) as $i |
    row(["`\($ids[$i])`"] + [$reports[] | .capabilities[$i] | labels[.status]])),
  row(["**Passing / v1 required**"] + [$reports[] as $r |
      [$r.capabilities[] | select($v1[.id] // [] | index($r.profile) != null)] |
      "\([.[] | select(.status == "pass")] | length)/\(length)"]),
  row(["**Verified / applicable**"] + [$reports[] | .capabilities |
      "\([.[] | select(.status == "pass")] | length)/\([.[] | select(.status != "not_applicable")] | length)"]),
  "", "Only N/A is excluded. Untested and unimplemented capabilities remain in the denominator.",
  "", ([$reports[] as $r | $r.capabilities[] | select(.reason != null) |
         "- \($r.profile) / `\(.id)`: \(.reason)"][]),
  "", "Candidate SHA-256: `\($candidate)` · checkout `\($revision)`",
  (if any($reports[] as $r | $r.capabilities[] |
          select($v1[.id] // [] | index($r.profile) != null); .status != "pass")
   then error("a required v1 capability was not verified") else empty end)
