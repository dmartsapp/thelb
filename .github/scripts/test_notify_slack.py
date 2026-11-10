#!/usr/bin/env python3
"""Tests for notify-slack.py: the message, the waiting, the posting, and that the
webhook URL can never end up in the repository. No network beyond a local server.

    python3 .github/scripts/test_notify_slack.py
"""
import contextlib
import glob
import importlib.util
import io
import json
import os
import subprocess
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
spec = importlib.util.spec_from_file_location("notify_slack", os.path.join(HERE, "notify-slack.py"))
ns = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ns)

TAG, REPO, SHA = "v4.0.6", "dmartsapp/theLB", "6607f211d296a90bb0a40ff92d278ef8aaf8e8ae"
SERVER = "https://github.com"


def run(name, conclusion="success", status="completed", rid=1, start="2026-09-21T10:00:00Z", end="2026-09-21T10:06:12Z"):
    return {"databaseId": rid, "workflowName": name, "status": status, "conclusion": conclusion if status == "completed" else "",
            "url": "%s/%s/actions/runs/%d" % (SERVER, REPO, rid), "startedAt": start, "updatedAt": end,
            "event": "push", "headBranch": TAG}


def all_runs(**overrides):
    runs = {}
    for i, (name, _) in enumerate(ns.WORKFLOWS):
        runs[name] = overrides.get(name, run(name, rid=100 + i))
    return runs


def payload(runs, timed_out=False, assets=14, release_url="https://github.com/dmartsapp/theLB/releases/tag/v4.0.6", failures=None):
    return ns.build_payload(TAG, REPO, SHA, "fix: something (v4.0.6)", "farhansabbir", SERVER, runs, timed_out, assets, release_url, failures or {})


def text_of(p):
    return json.dumps(p)


class MessageTests(unittest.TestCase):
    def test_all_passed(self):
        p = payload(all_runs())
        att = p["attachments"][0]
        self.assertEqual(att["color"], ns.GREEN)
        blocks = att["blocks"]
        head = blocks[0]["text"]["text"]
        self.assertIn("theLB v4.0.6 is released", head)
        self.assertIn("Every job passed", head)
        body = blocks[2]["text"]["text"]
        self.assertEqual(body.count(":white_check_mark:"), 1)
        self.assertIn("14 files attached: the Linux binaries, their SHA-256 sums and SBOMs", body)
        self.assertIn("6m 12s", body)
        buttons = blocks[-1]["elements"]
        self.assertEqual([b["text"]["text"] for b in buttons], ["Release notes", "Workflow runs"])
        self.assertIn("branch%3Av4.0.6", buttons[1]["url"])
        self.assertIn("/commit/" + SHA, text_of(p))
        self.assertIn("released", p["text"])

    def test_failure_names_what_failed_and_why_and_offers_no_release_button(self):
        runs = all_runs(Release=run("Release", conclusion="failure", rid=7))
        p = payload(runs, failures={"Release": ["check › make check", "why: internal/doctor/doctor_linux.go:705:4: QF1002 (staticcheck)", "why: make: *** [lint] Error 1"]})
        att = p["attachments"][0]
        self.assertEqual(att["color"], ns.RED)
        self.assertIn("release pipeline failed", att["blocks"][0]["text"]["text"])
        body = att["blocks"][2]["text"]["text"]
        self.assertIn(":x:", body)
        self.assertIn("check › make check", body)
        self.assertIn("why: make: *** [lint] Error 1", body)
        self.assertEqual([b["text"]["text"] for b in att["blocks"][-1]["elements"]], ["Workflow runs"])

    def test_missing_and_running_workflows(self):
        p = payload({}, timed_out=True, release_url=None)
        att = p["attachments"][0]
        self.assertEqual(att["color"], ns.AMBER)
        self.assertIn("did not start", att["blocks"][2]["text"]["text"])
        p = payload({"Release": run("Release", status="in_progress", rid=9)}, timed_out=True, release_url=None)
        self.assertIn("still running", p["attachments"][0]["blocks"][2]["text"]["text"])
        self.assertIn("still running", p["attachments"][0]["blocks"][0]["text"]["text"])

    def test_classify(self):
        names = [n for n, _ in ns.WORKFLOWS]
        self.assertEqual(ns.classify(all_runs(), False), ("passed", 1))
        self.assertEqual(ns.classify(all_runs(Release=run("Release", "cancelled")), False)[0], "failed")
        self.assertEqual(ns.classify(all_runs(Release=run("Release", "timed_out")), False)[0], "failed")
        self.assertEqual(ns.classify({}, True)[0], "running")
        self.assertEqual(ns.classify({names[0]: run(names[0], status="in_progress")}, False)[0], "running")

    def test_slack_limits(self):
        p = payload(all_runs(Release=run("Release", "failure", rid=3)),
                    failures={"Release": ["build (arm64) › Build " * 3] * 4})
        blocks = p["attachments"][0]["blocks"]
        self.assertLessEqual(len(blocks), 50)
        for b in blocks:
            if b["type"] == "section":
                self.assertLessEqual(len(b["text"]["text"]), 3000)
        self.assertLessEqual(len(json.dumps(p)), 30000)
        self.assertEqual(json.loads(json.dumps(p)), p)

    def test_long_subject_is_cut(self):
        p = ns.build_payload(TAG, REPO, SHA, "x" * 500, "a", SERVER, all_runs(), False, 14, None, {})
        ctx = p["attachments"][0]["blocks"][3]["elements"][0]["text"]
        self.assertLess(len(ctx), 300)

    def test_durations(self):
        self.assertEqual(ns.human(372), "6m 12s")
        self.assertEqual(ns.human(45), "45s")
        self.assertIsNone(ns.human(None))
        self.assertIsNone(ns.duration({"startedAt": "", "updatedAt": ""}))


class WaitTests(unittest.TestCase):
    def test_fetch_keeps_only_this_tags_newest_push_runs(self):
        listing = [
            run("Lint", rid=10), run("Lint", rid=12, conclusion="failure"),        # a re-run: newest wins
            dict(run("CodeQL", rid=11), headBranch="main"),                          # main's runs are not ours
            dict(run("Pages", rid=13), event="dynamic"),   # not a push: ignored
            run("Notify Slack", rid=99),                                              # ourselves
        ]
        got = ns.fetch_runs(lambda args: listing, REPO, SHA, TAG, own_run=99)
        self.assertEqual(sorted(got), ["Lint"])
        self.assertEqual(got["Lint"]["databaseId"], 12)

    def test_waits_until_everything_finished(self):
        names = [n for n, _ in ns.WORKFLOWS]
        states = iter([{}, {names[0]: run(names[0], status="in_progress")}, all_runs()])
        clock = [0]
        slept = []
        runs, timed_out = ns.wait_for_runs(lambda: next(states), names, 1000, 20,
                                           sleep=lambda s: (slept.append(s), clock.__setitem__(0, clock[0] + s)),
                                           clock=lambda: clock[0], log=lambda *_: None)
        self.assertFalse(timed_out)
        self.assertEqual(len(runs), 1)
        self.assertEqual(slept, [20, 20])

    def test_gives_up_at_the_deadline_and_says_so(self):
        names = [n for n, _ in ns.WORKFLOWS]
        clock = [0]
        runs, timed_out = ns.wait_for_runs(lambda: {}, names, 100, 30,
                                           sleep=lambda s: clock.__setitem__(0, clock[0] + s),
                                           clock=lambda: clock[0], log=lambda *_: None)
        self.assertTrue(timed_out)
        self.assertEqual(runs, {})

    def test_an_api_hiccup_is_retried(self):
        names = [n for n, _ in ns.WORKFLOWS]
        calls = []

        def fetch():
            calls.append(1)
            if len(calls) == 1:
                raise RuntimeError("HTTP 502")
            return all_runs()
        runs, timed_out = ns.wait_for_runs(fetch, names, 1000, 1, sleep=lambda s: None, clock=lambda: 0, log=lambda *_: None)
        self.assertFalse(timed_out)
        self.assertEqual(len(calls), 2)

    def test_failure_reason_is_the_failed_steps_last_lines(self):
        log = "\n".join([
            "check\tmake check\t2026-11-10T22:00:01.0000000Z ##[group]Run make check",
            "check\tmake check\t2026-11-10T22:00:05.1000000Z ==> golangci-lint",
            "check\tmake check\t2026-11-10T22:00:09.2000000Z internal/doctor/doctor_linux.go:705:4: QF1002: could use tagged switch (staticcheck)",
            "check\tmake check\t2026-11-10T22:00:09.3000000Z \x1b[31m1 issues:\x1b[0m",
            "check\tmake check\t2026-11-10T22:00:09.4000000Z make: *** [lint] Error 1",
            "check\tmake check\t2026-11-10T22:00:09.5000000Z ##[error]Process completed with exit code 2.",
        ])
        why = ns.failure_reason(lambda args: log, REPO, {"databaseId": 5}, lines=3)
        self.assertEqual(why, ["1 issues:", "make: *** [lint] Error 1", "Process completed with exit code 2."])

        def boom(args):
            raise RuntimeError("no log")
        self.assertEqual(ns.failure_reason(boom, REPO, {"databaseId": 5}), [])

    def test_failed_steps_names_job_and_step(self):
        jobs = {"jobs": [{"name": "golangci-lint", "conclusion": "failure", "steps": [{"name": "Set up job", "conclusion": "success"}, {"name": "golangci-lint", "conclusion": "failure"}]},
                         {"name": "other", "conclusion": "success", "steps": []}]}
        self.assertEqual(ns.failed_steps(lambda args: jobs, REPO, {"databaseId": 5}), ["golangci-lint › golangci-lint"])
        many = {"jobs": [{"name": "j%d" % i, "conclusion": "failure", "steps": []} for i in range(6)]}
        self.assertEqual(ns.failed_steps(lambda args: many, REPO, {"databaseId": 5})[-1], "+3 more")

        def boom(args):
            raise RuntimeError("no")
        self.assertEqual(ns.failed_steps(boom, REPO, {"databaseId": 5}), [])


class FakeSlack(BaseHTTPRequestHandler):
    replies = []   # (status, body) served in turn; the last one repeats
    seen = []

    def do_POST(self):
        FakeSlack.seen.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
        status, body = FakeSlack.replies[min(len(FakeSlack.seen) - 1, len(FakeSlack.replies) - 1)]
        self.send_response(status)
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, *args):
        pass


class PostTests(unittest.TestCase):
    def serve(self, replies):
        FakeSlack.replies, FakeSlack.seen = replies, []
        server = HTTPServer(("127.0.0.1", 0), FakeSlack)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        # a URL that looks like a real webhook, so a leak into the output is visible
        return "http://127.0.0.1:%d/services/TSECRET/BSECRET/XSECRETXSECRET" % server.server_address[1]

    def call(self, fn):
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            result = fn()
        return result, out.getvalue()

    def test_ok(self):
        url = self.serve([(200, "ok")])
        result, out = self.call(lambda: ns.post(url, {"text": "hi"}, sleep=lambda s: None))
        self.assertTrue(result)
        self.assertEqual(FakeSlack.seen, [{"text": "hi"}])
        self.assertNotIn("SECRET", out)

    def test_a_message_slack_refuses_is_not_retried(self):
        url = self.serve([(400, "invalid_payload")])
        result, out = self.call(lambda: ns.post(url, {"text": "hi"}, sleep=lambda s: None))
        self.assertFalse(result)
        self.assertEqual(len(FakeSlack.seen), 1)
        self.assertIn("400", out)
        self.assertNotIn("SECRET", out)

    def test_a_server_error_is_retried(self):
        url = self.serve([(500, "oops"), (500, "oops"), (200, "ok")])
        result, out = self.call(lambda: ns.post(url, {"text": "hi"}, sleep=lambda s: None))
        self.assertTrue(result)
        self.assertEqual(len(FakeSlack.seen), 3)

    def test_unreachable(self):
        url = "http://127.0.0.1:9/services/TSECRET/BSECRET/XSECRET"
        result, out = self.call(lambda: ns.post(url, {"text": "hi"}, attempts=2, sleep=lambda s: None))
        self.assertFalse(result)
        self.assertNotIn("SECRET", out)

    def test_main_without_a_webhook_posts_nothing_and_succeeds(self):
        calls = []
        result, out = self.call(lambda: ns.main({"REPO": REPO, "TAG": TAG, "SHA": SHA}, gh=lambda a: calls.append(a)))
        self.assertEqual(result, 0)
        self.assertEqual(calls, [])
        self.assertIn("not set", out)

    def test_main_end_to_end(self):
        url = self.serve([(200, "ok")])
        listing = list(all_runs().values())

        def gh(args):
            if args[:2] == ["run", "list"]:
                return listing
            if args[:2] == ["release", "view"]:
                return {"assets": [{}] * 14, "url": "https://github.com/dmartsapp/theLB/releases/tag/v4.0.6"}
            if args[0] == "api":
                return {"message": "fix: a thing (v4.0.6)\n\nbody"}
            raise AssertionError(args)
        env = {"REPO": REPO, "TAG": TAG, "SHA": SHA, "ACTOR": "farhansabbir", "SLACK_WEBHOOK_URL": url, "RUN_ID": "1"}
        result, out = self.call(lambda: ns.main(env, gh=gh, sleep=lambda s: None))
        self.assertEqual(result, 0)
        sent = FakeSlack.seen[0]
        self.assertIn("released", sent["text"])
        self.assertIn("14 files attached", json.dumps(sent))
        self.assertIn("fix: a thing (v4.0.6)", json.dumps(sent))
        self.assertNotIn("SECRET", out)

    def test_main_reports_delivery_failure(self):
        url = self.serve([(404, "no_service")])
        env = {"REPO": REPO, "TAG": TAG, "SHA": SHA, "SLACK_WEBHOOK_URL": url}
        def gh(args):
            if args[:2] == ["run", "list"]:
                return list(all_runs().values())
            raise RuntimeError("no release, no commit")   # both are optional in the message
        result, _ = self.call(lambda: ns.main(env, gh=gh, sleep=lambda s: None))
        self.assertEqual(result, 1)


class RepositoryTests(unittest.TestCase):
    def workflow_files(self):
        return sorted(glob.glob(os.path.join(ROOT, ".github", "workflows", "*.yaml")))

    NOT_PART_OF_THE_RELEASE = set()

    def test_the_list_matches_the_release_workflows(self):
        """Every workflow a tag starts (except this notifier and the README proposal) is waited for, and nothing else is."""
        tagged = set()
        for path in self.workflow_files():
            with open(path, encoding="utf-8") as f:
                source = f.read()
            if os.path.basename(path) in ({"notify-slack.yaml"} | self.NOT_PART_OF_THE_RELEASE) or "tags:" not in source:
                continue
            first = next(l for l in source.splitlines() if l.startswith("name:"))
            tagged.add(first.split(":", 1)[1].strip())
        self.assertEqual(tagged, {n for n, _ in ns.WORKFLOWS})

    def test_the_workflow_reads_the_secret_and_nothing_else(self):
        with open(os.path.join(ROOT, ".github", "workflows", "notify-slack.yaml"), encoding="utf-8") as f:
            source = f.read()
        self.assertIn("secrets.SLACK_WEBHOOK_URL", source)
        self.assertNotIn("${{ github.event", source)   # no event text interpolated anywhere
        on_block = source[source.index("\non:"):source.index("\njobs:")]
        self.assertNotIn("workflow_run", on_block)      # the trigger rule allows only tag pushes

    def test_no_slack_webhook_url_is_committed(self):
        needle = "hooks.slack" + ".com/services/"
        listing = subprocess.run(["git", "ls-files"], cwd=ROOT, capture_output=True, text=True)
        if listing.returncode != 0:
            self.skipTest("not a git checkout")
        leaks = []
        for rel in listing.stdout.splitlines():
            path = os.path.join(ROOT, rel)
            try:
                with open(path, encoding="utf-8", errors="ignore") as f:
                    if needle in f.read():
                        leaks.append(rel)
            except OSError:
                pass
        self.assertEqual(leaks, [], "a Slack webhook URL is a credential: keep it in the SLACK_WEBHOOK_URL secret")


if __name__ == "__main__":
    unittest.main()
