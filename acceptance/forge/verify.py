"""Verify test2json results without publishing CLI output or fixture credentials."""
import argparse
import json
from pathlib import Path
import re
import sys

PACKAGE = "github.com/updatecli/updatecli/acceptance/forge"


def verify(provider, cases, events):
    root = f"TestForge/{provider}"
    leaves = {f"{root}/{name}" for name in cases}
    if not cases or len(leaves) != len(cases) or not all(re.fullmatch(r"[a-z0-9_/-]+", name) for name in cases):
        raise ValueError("invalid case inventory")
    paths = {leaf[:i] for leaf in leaves for i, c in enumerate(leaf) if c == "/"} | leaves
    started, passed, failed = set(), set(), set()
    locations = {}
    package_started = package_passed = False
    for line in events:
        event = json.loads(line)
        if event.get("Package") != PACKAGE or package_passed:
            raise ValueError("foreign test event or output after package pass")
        test, action = event.get("Test", ""), event["Action"]
        if test and test not in paths:
            raise ValueError("unexpected test name; check cases.txt")
        if action == "start" and not test:
            if package_started:
                raise ValueError("duplicate package start")
            package_started = True
        elif not package_started:
            raise ValueError("missing package start")
        elif action == "run":
            if not test or test in started:
                raise ValueError(f"unexpected or duplicate run: {test}")
            started.add(test)
        elif action == "output":
            # Show a source location for failures, not arbitrary child output.
            match = re.search(r"\b([a-z_]+_test\.go:\d+):", event.get("Output", ""))
            if match:
                locations[test] = match[1]
        elif action == "pass" and test:
            if test not in started or test in passed or test in failed:
                raise ValueError(f"unexpected or duplicate pass: {test}")
            passed.add(test)
        elif action in ("skip", "fail"):
            failed.add(test or PACKAGE)
        elif action == "pass" and not test:
            package_passed = True
    if failed:
        # Include a copyable rerun for the most specific failed test, not only its parents.
        names = failed - {PACKAGE}
        test = max(sorted(names), key=lambda name: name.count("/"), default=root)
        match = "/".join(f"^{part}$" for part in test.split("/"))
        errors = ", ".join(f"{name} ({locations.get(name, 'see local run')})" for name in sorted(failed))
        raise ValueError(f"{errors}\nWith UPDATECLI_BIN set, run: UPDATECLI_TEST_FORGE={provider} "
                         f"go test -tags=acceptance -count=1 -v -run '{match}' ./acceptance/forge")
    if not package_passed or started != passed or not ({"TestForge", root} | leaves) <= passed:
        missing = ", ".join(sorted(leaves - passed))
        raise ValueError(f"incomplete test run; missing {missing or 'package or parent pass'}")
    print(f"{provider}: {len(leaves)} named cases passed")
    for name in sorted(cases):
        print(f"  PASS {name}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("provider", choices=["gitea", "forgejo"])
    args = parser.parse_args()
    try:
        verify(args.provider, Path(__file__).with_name("cases.txt").read_text().splitlines(), sys.stdin)
    except (OSError, ValueError, KeyError) as exc:
        sys.exit(f"forge acceptance: {exc}")
