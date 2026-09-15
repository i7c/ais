#!/usr/bin/env python3
"""Unit tests for the parts of ais that are awkward to exercise by running it.

    python3 test_ais.py

The session guard is the reason this file exists: deciding whether a hook may
speak for a session depends on process ancestry, which a test cannot stage
without spawning real harness processes. Here the ancestry is handed in.
"""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_loader(
    "ais", importlib.machinery.SourceFileLoader("ais", str(Path(__file__).with_name("ais")))
)
ais = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ais)


class SpeaksFor(unittest.TestCase):
    """Only the harness process ais started may report a conversation id.

    The command names are the ones macOS actually reports: claude is a real
    binary, so ps gives "claude", while ais itself shows up as its interpreter.
    """

    session = {"pid": 500, "harness": "claude", "command": ["claude", "--session-id", "x"]}

    def setUp(self):
        self.real_ancestry = ais.ancestry

    def tearDown(self):
        ais.ancestry = self.real_ancestry

    def chain(self, ancestry):
        ais.ancestry = lambda: ancestry
        return ais.speaks_for(self.session)

    def test_hook_under_the_session_harness(self):
        # sh (the hook) -> claude (ours) -> ais
        self.assertTrue(self.chain([(499, "sh"), (500, "claude"), (400, "Python")]))

    def test_nested_harness_may_not_speak(self):
        # a "claude -p" the agent spawned: a nearer claude than ours
        self.assertFalse(
            self.chain([(700, "sh"), (699, "claude"), (500, "claude"), (400, "Python")])
        )

    def test_unrelated_process_may_not_speak(self):
        # AIS_SESSION exported into some other shell, nothing to do with us
        self.assertFalse(self.chain([(900, "sh"), (800, "zsh"), (1, "launchd")]))

    def test_unreadable_ancestry_is_allowed(self):
        # ps failed: assume the common case rather than silently recording nothing
        self.assertTrue(self.chain([]))

    def test_no_recorded_pid_is_allowed(self):
        ais.ancestry = lambda: [(1, "launchd")]
        self.assertTrue(ais.speaks_for({"harness": "claude"}))


class ArgvHandling(unittest.TestCase):
    conflicts = ["-c", "--continue", "--fork-session", "--resume", "--session-id", "-r"]

    def test_spots_a_flag_you_typed(self):
        self.assertEqual(ais.names_a_session(["--resume", "abc"], self.conflicts), "--resume")
        self.assertEqual(ais.names_a_session(["--resume=abc"], self.conflicts), "--resume")
        self.assertEqual(ais.names_a_session(["-c"], self.conflicts), "-c")

    def test_leaves_ordinary_args_alone(self):
        self.assertIsNone(ais.names_a_session(["--model", "opus"], self.conflicts))

    def test_stops_at_a_bare_dashdash(self):
        # past "--" the tokens belong to whatever the harness passes on
        self.assertIsNone(ais.names_a_session(["--", "--resume", "x"], self.conflicts))

    def test_strips_recorded_session_flags(self):
        self.assertEqual(
            ais.strip_known_flags(["--model", "opus", "--session-id", "u"], self.conflicts),
            ["--model", "opus"],
        )
        self.assertEqual(
            ais.strip_known_flags(["--session-id=u", "-p"], self.conflicts), ["-p"]
        )


class SessionConfig(unittest.TestCase):
    def test_templates_must_carry_the_id(self):
        with self.assertRaises(SystemExit):
            ais.session_settings({"new": ["--session-id"]}, "claude")

    def test_conflicts_collect_every_session_flag(self):
        conf = ais.session_settings(
            {"new": ["--session-id", "{id}"], "resume": ["--resume", "{id}"],
             "conflicts": ["-c"]},
            "claude",
        )
        self.assertEqual(conf["conflicts"], ["--resume", "--session-id", "-c"])

    def test_absent_table_is_not_an_error(self):
        self.assertIsNone(ais.session_settings(None, "pi.dev"))

    def test_id_kind_is_checked(self):
        with self.assertRaises(SystemExit):
            ais.session_settings({"id": "sequential"}, "claude")

    def test_minted_ids_look_right(self):
        self.assertEqual(len(ais.mint_session_id("uuid4").split("-")), 5)
        self.assertEqual(len(ais.mint_session_id("short")), ais.ID_LENGTH)


class IdHistory(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        home = Path(self.tmp.name)
        ais.AIS_HOME = home
        ais.SESSIONS_PATH = home / "sessions.json"
        ais.LOCK_PATH = home / "sessions.lock"
        ais.write_sessions([{"id": "abc", "session_ids": []}])

    def tearDown(self):
        self.tmp.cleanup()

    def ids(self):
        return json.loads(ais.SESSIONS_PATH.read_text())["sessions"][0]["session_ids"]

    def test_appends_a_new_conversation(self):
        ais.remember_session_id("abc", "one")
        ais.remember_session_id("abc", "two")  # /clear started a new one
        self.assertEqual(self.ids(), ["one", "two"])

    def test_repeating_the_current_one_changes_nothing(self):
        ais.remember_session_id("abc", "one")
        ais.remember_session_id("abc", "one")
        self.assertEqual(self.ids(), ["one"])

    def test_history_is_capped(self):
        for i in range(ais.SESSION_ID_KEEP + 5):
            ais.remember_session_id("abc", f"id-{i}")
        self.assertEqual(len(self.ids()), ais.SESSION_ID_KEEP)
        self.assertEqual(ais.harness_session_id({"session_ids": self.ids()}), "id-24")

    def test_unknown_session_is_reported(self):
        self.assertFalse(ais.remember_session_id("nope", "one"))


class ClientResolution(unittest.TestCase):
    """select must come out of this holding the name of a client that exists.

    The case worth pinning is a key binding passing "#{client_name}" through
    display-popup, which hands its command to the shell unexpanded: the format
    arrives literally, and taking it at face value means switch-client dies on
    a client name that was never a client at all — in a popup that closes over
    the error. Anything tmux does not recognise is dropped for what tmux itself
    would pick, which inside a popup is still the client the key came from.
    """

    clients = ["/dev/ttys012", "/dev/ttys003"]

    def setUp(self):
        self.real_tmux = ais.tmux
        self.current = "/dev/ttys012"
        self.rc = 0
        ais.tmux = self.fake_tmux
        self.real_env = os.environ.get("TMUX")
        os.environ["TMUX"] = "/tmp/tmux-502/default,1,0"

    def tearDown(self):
        ais.tmux = self.real_tmux
        if self.real_env is None:
            os.environ.pop("TMUX", None)
        else:
            os.environ["TMUX"] = self.real_env

    def fake_tmux(self, *args, check=False):
        argv = list(args)
        if argv[0] == "list-clients":
            return subprocess.CompletedProcess(argv, 0, "\n".join(self.clients) + "\n", "")
        if argv[0] == "display-message":
            return subprocess.CompletedProcess(argv, self.rc, f"{self.current}\n", "")
        raise AssertionError(f"unexpected tmux call: {argv}")

    def test_a_live_client_is_taken_as_given(self):
        self.assertEqual(ais.resolve_client("/dev/ttys003"), "/dev/ttys003")

    def test_an_unexpanded_format_falls_back(self):
        self.assertEqual(ais.resolve_client("#{client_name}"), "/dev/ttys012")

    def test_a_client_that_has_gone_falls_back(self):
        self.assertEqual(ais.resolve_client("/dev/ttys999"), "/dev/ttys012")

    def test_no_name_asks_tmux(self):
        self.assertEqual(ais.resolve_client(None), "/dev/ttys012")

    def test_outside_tmux_there_is_no_client(self):
        del os.environ["TMUX"]
        self.assertIsNone(ais.resolve_client(None))
        self.assertIsNone(ais.resolve_client("#{client_name}"))

    def test_tmux_answering_nothing_is_no_client(self):
        self.current = ""
        self.assertIsNone(ais.resolve_client(None))

    def test_a_failed_query_is_no_client(self):
        self.rc = 1
        self.assertIsNone(ais.resolve_client(None))

    def test_without_a_client_there_is_no_pane(self):
        # no client to ask, so no "here" to mark: a missing marker, not an error
        self.assertIsNone(ais.client_pane(None))


class HereMarker(unittest.TestCase):
    """"You are here" is the client's pane matched against the recorded ones."""

    sessions = [
        {"id": "aaa", "tmux": {"pane_id": "%1"}},
        {"id": "bbb", "tmux": {"pane_id": "%2"}},
        {"id": "ccc"},  # never recorded a pane
    ]

    def test_matches_the_recorded_pane(self):
        self.assertEqual(ais.session_at_pane("%2", self.sessions), "bbb")

    def test_a_pane_that_is_no_session_marks_nothing(self):
        self.assertIsNone(ais.session_at_pane("%9", self.sessions))

    def test_no_pane_marks_nothing(self):
        self.assertIsNone(ais.session_at_pane(None, self.sessions))


class ClaudeMdBlock(unittest.TestCase):
    """install must own its own section and nothing else in the file."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        os.environ["CLAUDE_CONFIG_DIR"] = self.tmp.name
        self.path = Path(self.tmp.name) / "CLAUDE.md"

    def tearDown(self):
        del os.environ["CLAUDE_CONFIG_DIR"]
        self.tmp.cleanup()

    def test_appends_after_existing_notes(self):
        self.path.write_text("# My notes\n\nkeep me\n")
        ais.install_claude_md(dry=False)
        text = self.path.read_text()
        self.assertIn("keep me", text)
        self.assertIn(ais.CLAUDE_MD_BEGIN, text)
        self.assertTrue(text.rstrip().endswith(ais.CLAUDE_MD_END))

    def test_replaces_only_its_own_block(self):
        self.path.write_text(
            f"before\n\n{ais.CLAUDE_MD_BEGIN}\nstale text\n{ais.CLAUDE_MD_END}\n\nafter\n"
        )
        ais.install_claude_md(dry=False)
        text = self.path.read_text()
        self.assertNotIn("stale text", text)
        self.assertIn("before", text)
        self.assertIn("after", text)
        self.assertEqual(text.count(ais.CLAUDE_MD_BEGIN), 1)

    def test_dry_run_writes_nothing(self):
        self.path.write_text("untouched\n")
        ais.install_claude_md(dry=True)
        self.assertEqual(self.path.read_text(), "untouched\n")


if __name__ == "__main__":
    unittest.main(verbosity=2)
