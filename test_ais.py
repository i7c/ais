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
import tomllib
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
        self.assertIsNone(ais.session_settings(None, "aider"))

    def test_the_shipped_tables_are_valid(self):
        """Every block "ais install" writes has to survive being read back."""
        for harness, block in (
            ("claude", ais.CLAUDE_SESSION_CONFIG), ("pi", ais.PI_SESSION_CONFIG),
        ):
            table = tomllib.loads(block)["harness"][harness]["session"]
            conf = ais.session_settings(table, harness)
            self.assertIn("--session-id", conf["conflicts"])
            self.assertTrue(conf["new"] and conf["resume"])

    def test_pi_reopens_with_the_flag_it_was_created_with(self):
        """pi's --session-id creates when there is none and opens when there is,
        so new and resume are the same flag and restart still lands."""
        table = tomllib.loads(ais.PI_SESSION_CONFIG)["harness"]["pi"]["session"]
        conf = ais.session_settings(table, "pi")
        self.assertEqual(ais.session_argv(conf["resume"], "abc"), ["--session-id", "abc"])
        for typed in ("-c", "--continue", "-r", "--resume", "--session", "--fork"):
            self.assertEqual(ais.names_a_session([typed], conf["conflicts"]), typed)

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


class WindowName(unittest.TestCase):
    """A session names its window, and leaves the window as it found it.

    The restore is the part worth pinning: automatic-rename has to go back to
    what the window had, and "nothing at all" is a value tmux only accepts as
    an unset, not as the value the window was inheriting.
    """

    def setUp(self):
        self.real_tmux = ais.tmux
        self.calls = []
        self.auto = ""      # what show-window-options reports: unset
        self.name = "zsh"   # the window name before we touch it
        self.fails = None   # a command that refuses, for the failure cases
        ais.tmux = self.fake_tmux

    def tearDown(self):
        ais.tmux = self.real_tmux

    def fake_tmux(self, *args, check=False):
        argv = list(args)
        self.calls.append(argv)
        if argv[0] == self.fails:
            return subprocess.CompletedProcess(argv, 1, "", "no")
        out = ""
        if argv[0] == "display-message":
            out = self.name + "\n"
        elif argv[0] == "show-window-options":
            out = (self.auto + "\n") if self.auto else ""
        return subprocess.CompletedProcess(argv, 0, out, "")

    def run_named(self, name="fix-the-auth-bug"):
        with ais.window_named("%3", name) as renamed:
            self.calls.append(["<running>"])
        return renamed

    def test_names_the_window_and_holds_it(self):
        self.assertTrue(self.run_named())
        self.assertIn(["set-window-option", "-t", "%3", "automatic-rename", "off"], self.calls)
        self.assertIn(["rename-window", "-t", "%3", "fix-the-auth-bug"], self.calls)

    def test_puts_the_old_name_back_afterwards(self):
        self.run_named()
        after = self.calls[self.calls.index(["<running>"]):]
        self.assertIn(["rename-window", "-t", "%3", "zsh"], after)
        self.assertIn(["set-window-option", "-u", "-t", "%3", "automatic-rename"], after)

    def test_an_option_the_window_had_is_restored_as_it_was(self):
        self.auto = "on"
        self.run_named()
        self.assertIn(
            ["set-window-option", "-t", "%3", "automatic-rename", "on"], self.calls
        )
        self.assertNotIn(["set-window-option", "-u", "-t", "%3", "automatic-rename"], self.calls)

    def test_a_window_that_refuses_is_left_alone(self):
        # the pane went away mid-start: nothing was renamed, so nothing is undone
        self.fails = "rename-window"
        self.assertFalse(self.run_named())
        after = self.calls[self.calls.index(["<running>"]):]
        self.assertEqual(after, [["<running>"]])

    def test_restores_even_when_the_session_blows_up(self):
        with self.assertRaises(SystemExit):
            with ais.window_named("%3", "boom"):
                raise SystemExit(1)
        self.assertIn(["rename-window", "-t", "%3", "zsh"], self.calls[3:])

    def test_the_session_name_is_the_window_name(self):
        self.assertEqual(ais.window_name({"slug": "auth-bug", "harness": "claude"}), "auth-bug")

    def test_a_description_names_it_before_a_slug_exists(self):
        self.assertEqual(
            ais.window_name({"description": "fix the auth bug", "harness": "claude"}),
            "fix-auth-bug",
        )

    def test_a_nameless_session_falls_back_to_the_harness(self):
        self.assertEqual(ais.window_name({"harness": "claude"}), "claude")


class RenameSetting(unittest.TestCase):
    def test_renaming_is_on_unless_turned_off(self):
        self.assertTrue(ais.rename_window_setting({}))
        self.assertTrue(ais.rename_window_setting({"tmux": {}}))
        self.assertFalse(ais.rename_window_setting({"tmux": {"rename": False}}))

    def test_a_non_boolean_is_refused(self):
        with self.assertRaises(SystemExit):
            ais.rename_window_setting({"tmux": {"rename": "off"}})


class Quietly(unittest.TestCase):
    """Base for the install tests: they are about the files, not the report."""

    def setUp(self):
        self.real_report = ais.report
        ais.report = lambda *a, **k: None

    def tearDown(self):
        ais.report = self.real_report


class InstructionsBlock(Quietly):
    """install must own its own section and nothing else in the file.

    Same markers, same code, two files: ~/.claude/CLAUDE.md for claude and
    ~/.pi/agent/AGENTS.md for pi.
    """

    def setUp(self):
        super().setUp()
        self.tmp = tempfile.TemporaryDirectory()
        os.environ["CLAUDE_CONFIG_DIR"] = self.tmp.name
        os.environ["PI_CODING_AGENT_DIR"] = self.tmp.name
        self.path = Path(self.tmp.name) / "CLAUDE.md"

    def tearDown(self):
        super().tearDown()
        del os.environ["CLAUDE_CONFIG_DIR"]
        del os.environ["PI_CODING_AGENT_DIR"]
        self.tmp.cleanup()

    def install(self, harness="claude", dry=False):
        ais.install_instructions(harness, self.path, dry)

    def test_appends_after_existing_notes(self):
        self.path.write_text("# My notes\n\nkeep me\n")
        self.install()
        text = self.path.read_text()
        self.assertIn("keep me", text)
        self.assertIn(ais.SECTION_BEGIN, text)
        self.assertTrue(text.rstrip().endswith(ais.SECTION_END))

    def test_replaces_only_its_own_block(self):
        self.path.write_text(
            f"before\n\n{ais.SECTION_BEGIN}\nstale text\n{ais.SECTION_END}\n\nafter\n"
        )
        self.install()
        text = self.path.read_text()
        self.assertNotIn("stale text", text)
        self.assertIn("before", text)
        self.assertIn("after", text)
        self.assertEqual(text.count(ais.SECTION_BEGIN), 1)

    def test_dry_run_writes_nothing(self):
        self.path.write_text("untouched\n")
        self.install(dry=True)
        self.assertEqual(self.path.read_text(), "untouched\n")

    def test_a_harness_switch_replaces_the_section(self):
        """The same markers, so re-pointing a file at another harness is a
        replacement, not a second copy."""
        self.install("claude")
        self.install("pi")
        text = self.path.read_text()
        self.assertEqual(text.count(ais.SECTION_BEGIN), 1)
        self.assertIn("`ais -- pi`", text)
        self.assertNotIn("`ais -- claude`", text)

    def test_pi_is_where_install_puts_it(self):
        self.assertEqual(ais.HARNESSES["pi"]["instructions"](), Path(self.tmp.name) / "AGENTS.md")
        self.assertEqual(ais.HARNESSES["claude"]["instructions"](), Path(self.tmp.name) / "CLAUDE.md")


class RenderedInstructions(unittest.TestCase):
    """One text, two harnesses: only the sentences that name one may differ."""

    def test_each_harness_is_told_its_own_name(self):
        for harness in ("claude", "pi"):
            rendered = ais.instructions(harness)
            self.assertIn(f"normally `{harness}`", rendered)
            self.assertIn(f"no `ais -- {harness}`", rendered)
            self.assertNotIn("{harness}", rendered)
            self.assertNotIn("{notify_lead}", rendered)

    def test_only_claude_is_warned_off_its_own_notifier(self):
        # pi has no LLM-callable notification tool to be warned off.
        self.assertIn("built-in\nnotification tool", ais.instructions("claude"))
        self.assertNotIn("notification tool", ais.instructions("pi"))

    def test_the_rules_themselves_are_shared(self):
        shared = "NEVER run `ais select`"
        self.assertIn(shared, ais.instructions("claude"))
        self.assertIn(shared, ais.instructions("pi"))


class PiExtension(Quietly):
    """pi has no hook table, so ais owns a file in its extensions directory."""

    def setUp(self):
        super().setUp()
        self.tmp = tempfile.TemporaryDirectory()
        os.environ["PI_CODING_AGENT_DIR"] = self.tmp.name
        self.path = Path(self.tmp.name) / "extensions" / "ais.ts"

    def tearDown(self):
        super().tearDown()
        del os.environ["PI_CODING_AGENT_DIR"]
        self.tmp.cleanup()

    def test_writes_the_extension(self):
        ais.install_extension(dry=False, force=False)
        text = self.path.read_text()
        self.assertIn("session_start", text)
        self.assertIn("_session-seen", text)
        self.assertIn(ais.PI_EXTENSION_MARKER, text)

    def test_dry_run_writes_nothing(self):
        ais.install_extension(dry=True, force=False)
        self.assertFalse(self.path.exists())

    def test_rewrites_its_own_file_when_it_moves_on(self):
        ais.install_extension(dry=False, force=False)
        self.path.write_text(self.path.read_text().replace("detached: true", "detached: false"))
        ais.install_extension(dry=False, force=False)
        self.assertEqual(self.path.read_text(), ais.pi_extension())

    def test_leaves_a_file_it_did_not_write_alone(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text("// someone else's extension\n")
        ais.install_extension(dry=False, force=False)
        self.assertEqual(self.path.read_text(), "// someone else's extension\n")

    def test_force_takes_a_foreign_file_over(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text("// someone else's extension\n")
        ais.install_extension(dry=False, force=True)
        self.assertEqual(self.path.read_text(), ais.pi_extension())
        self.assertIn("someone else", self.path.with_name("ais.ts.ais.bak").read_text())

    def test_the_ais_it_calls_is_quoted_for_javascript(self):
        # A path with a space in it would otherwise end the string literal.
        real = ais.ais_command
        ais.ais_command = lambda: "/Applications/My Tools/ais"
        try:
            self.assertIn('spawn("/Applications/My Tools/ais"', ais.pi_extension())
        finally:
            ais.ais_command = real


class InstallConfig(Quietly):
    """The session table goes in once, whichever harness asks for it."""

    def setUp(self):
        super().setUp()
        self.tmp = tempfile.TemporaryDirectory()
        os.environ["AIS_HOME"] = self.tmp.name
        self.real = ais.AIS_HOME, ais.CONFIG_PATH
        ais.AIS_HOME = Path(self.tmp.name)
        ais.CONFIG_PATH = ais.AIS_HOME / "config.toml"

    def tearDown(self):
        super().tearDown()
        ais.AIS_HOME, ais.CONFIG_PATH = self.real
        del os.environ["AIS_HOME"]
        self.tmp.cleanup()

    def test_adds_then_leaves_alone(self):
        ais.install_config("pi", ais.PI_SESSION_CONFIG, dry=False)
        once = ais.CONFIG_PATH.read_text()
        self.assertIn("[harness.pi.session]", once)
        ais.install_config("pi", ais.PI_SESSION_CONFIG, dry=False)
        self.assertEqual(ais.CONFIG_PATH.read_text(), once)

    def test_the_starter_config_does_not_count_as_installed(self):
        """It ships the table commented out as documentation."""
        ais.install_config("claude", ais.CLAUDE_SESSION_CONFIG, dry=False)
        self.assertIn('resume = ["--resume", "{id}"]', ais.CONFIG_PATH.read_text())

    def test_both_harnesses_can_be_wired_up_at_once(self):
        ais.install_config("claude", ais.CLAUDE_SESSION_CONFIG, dry=False)
        ais.install_config("pi", ais.PI_SESSION_CONFIG, dry=False)
        config = tomllib.loads(ais.CONFIG_PATH.read_text())
        self.assertEqual(config["harness"]["claude"]["session"]["resume"], ["--resume", "{id}"])
        self.assertEqual(config["harness"]["pi"]["session"]["resume"], ["--session-id", "{id}"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
