import contextlib
import io
import json
import unittest

from verify import PACKAGE, verify


def stream(*events):
    return [json.dumps({"Package": PACKAGE, "Action": action, **({"Test": name} if name else {})})
            for action, name in events]


class TestForgeResults(unittest.TestCase):
    def setUp(self):
        names = ["TestForge", "TestForge/gitea", "TestForge/gitea/publish"]
        self.events = stream(("start", ""), *(("run", n) for n in names),
                             *(("pass", n) for n in reversed(names)), ("pass", ""))

    def check(self, events):
        with contextlib.redirect_stdout(io.StringIO()):
            verify("gitea", ["publish"], events)

    def test_complete(self):
        self.check(self.events)

    def test_invalid(self):
        for reason, events in {
            "truncated": self.events[:-1],
            "filtered": self.events[:3] + self.events[5:],
            "duplicate": self.events[:4] + [self.events[3]] + self.events[4:],
            "skip": [s.replace('"pass"', '"skip"') if "/publish" in s else s for s in self.events],
            "foreign": [s.replace("/gitea", "/forgejo") for s in self.events],
            "foreign package": [s.replace(PACKAGE, "other") for s in self.events],
            "missing start": self.events[1:],
            "after completion": self.events + [self.events[-1]],
        }.items():
            with self.subTest(reason=reason), self.assertRaises(ValueError):
                self.check(events)
