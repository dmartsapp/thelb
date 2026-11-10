#!/usr/bin/env python3
"""Post the outcome of a release tag's workflows to Slack, as one clean message.

    python3 .github/scripts/notify-slack.py

Run by .github/workflows/notify-slack.yaml, which the tag push starts alongside
the five release workflows. This waits until each of them has finished (or the
wait runs out), then posts one message: the tag, whether everything passed, one
line per workflow with its duration and a link to its run, what failed and where,
and buttons for the release and the runs.

Settings come from the environment:

  GH_TOKEN            token for the gh CLI (actions: read, contents: read)
  REPO, TAG, SHA      owner/name, the tag (v4.0.6) and the commit it points at
  RUN_ID              this workflow's own run, ignored while waiting
  ACTOR               who pushed the tag
  SERVER_URL          https://github.com
  SLACK_WEBHOOK_URL   the incoming webhook; without it nothing is posted (a fork,
                      or the secret not set) and the run still succeeds
  WAIT_SECONDS        how long to wait for the workflows (default 3000)
  POLL_SECONDS        seconds between looks (default 20)
  DRY_RUN=1           print the message instead of posting it

The webhook URL is a credential: it is never printed, logged or put in an error.
Standard library only, like the other scripts here.
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime

# The workflows a release tag starts, in the order they are shown:
# (the workflow's `name:`, the label shown in Slack). A test keeps this list in
# step with the workflow files, so adding a release workflow without listing it
# here fails `make workflows`.
WORKFLOWS = [
    ("Release", "Release: checks, end-to-end test, Linux builds, publishing"),
]

GREEN, RED, AMBER = "#2EB67D", "#E01E5A", "#ECB22E"
OK_CONCLUSIONS = ("success",)
NOT_RUN_CONCLUSIONS = ("skipped", "neutral")


def parse_time(text):
    try:
        return datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ")
    except (TypeError, ValueError):
        return None


def duration(run):
    start, end = parse_time(run.get("startedAt")), parse_time(run.get("updatedAt"))
    return int((end - start).total_seconds()) if start and end and end >= start else None


def human(seconds):
    if seconds is None:
        return None
    minutes, secs = divmod(int(seconds), 60)
    return "%dm %02ds" % (minutes, secs) if minutes else "%ds" % secs


def gh_json(args):
    """Run the gh CLI and parse its JSON output."""
    done = subprocess.run(["gh"] + list(args), capture_output=True, text=True)
    if done.returncode != 0:
        raise RuntimeError("gh %s failed: %s" % (" ".join(args[:2]), done.stderr.strip()[:200]))
    return json.loads(done.stdout or "null")


def fetch_runs(gh, repo, sha, tag, own_run):
    """The newest run of each workflow that this tag push started."""
    runs = gh(["run", "list", "--repo", repo, "--commit", sha, "--limit", "100", "--json",
               "databaseId,workflowName,status,conclusion,url,startedAt,updatedAt,event,headBranch"])
    latest = {}
    for run in runs or []:
        if run.get("event") != "push" or run.get("headBranch") != tag or str(run["databaseId"]) == str(own_run):
            continue
        name = run["workflowName"]
        if name not in latest or run["databaseId"] > latest[name]["databaseId"]:
            latest[name] = run
    return latest


def wait_for_runs(fetch, names, wait_seconds, poll_seconds, sleep=time.sleep, clock=time.monotonic, log=print):
    """Poll until every named workflow has completed. Returns (runs, timed_out)."""
    started = clock()
    runs = {}
    while True:
        try:
            runs = fetch()
        except RuntimeError as err:  # a hiccup in the API: look again rather than give up
            log("could not list the runs (%s); will retry" % err)
        pending = [n for n in names if n not in runs or runs[n].get("status") != "completed"]
        if not pending:
            return runs, False
        if clock() - started >= wait_seconds:
            return runs, True
        log("waiting for: " + ", ".join("%s (%s)" % (n, runs[n]["status"] if n in runs else "not started") for n in pending))
        sleep(poll_seconds)


def failure_reason(gh_text, repo, run, lines=4):
    """The last lines of what failed - the failed steps' log, without timestamps,
    colours or GitHub's markers - to say why, not only where."""
    try:
        log = gh_text(["run", "view", str(run["databaseId"]), "--repo", repo, "--log-failed"])
    except RuntimeError:
        return []
    out = []
    for raw in log.splitlines():
        text = raw.split("\t", 2)[-1]
        text = re.sub(r"^\ufeff?\d{4}-\d\d-\d\dT[\d:.]+Z ?", "", text)
        text = re.sub(r"\x1b\[[0-9;]*m", "", text).replace("##[error]", "").strip()
        if text and not text.startswith(("##[group]", "##[endgroup]", "shell:", "env:")):
            out.append(text if len(text) <= 160 else text[:157] + "...")
    return out[-lines:]


def failed_steps(gh, repo, run, limit=3):
    """'job > step' for what failed in a run, at most limit of them."""
    try:
        jobs = gh(["run", "view", str(run["databaseId"]), "--repo", repo, "--json", "jobs"])["jobs"]
    except (RuntimeError, KeyError, TypeError):
        return []
    found = []
    for job in jobs:
        if job.get("conclusion") != "failure":
            continue
        steps = [s["name"] for s in job.get("steps", []) if s.get("conclusion") == "failure"]
        found.append("%s › %s" % (job["name"], steps[0]) if steps else job["name"])
    return found[:limit] + (["+%d more" % (len(found) - limit)] if len(found) > limit else [])


def classify(runs, timed_out):
    """('passed' | 'failed' | 'running', number passed)."""
    conclusions = [runs[n]["conclusion"] if n in runs and runs[n].get("status") == "completed" else None
                   for n, _ in WORKFLOWS]
    passed = sum(1 for c in conclusions if c in OK_CONCLUSIONS)
    if any(c is not None and c not in OK_CONCLUSIONS and c not in NOT_RUN_CONCLUSIONS for c in conclusions):
        return "failed", passed
    if any(c is None for c in conclusions) or timed_out:
        return "running", passed
    return ("passed" if passed == len(WORKFLOWS) else "failed"), passed


def line_for(label, run, extra, failures):
    if run is None:
        return ":grey_question:  *%s*  ·  did not start" % label
    if run.get("status") != "completed":
        icon, state = ":hourglass_flowing_sand:", "still running"
    elif run["conclusion"] in OK_CONCLUSIONS:
        icon, state = ":white_check_mark:", None
    elif run["conclusion"] in NOT_RUN_CONCLUSIONS:
        icon, state = ":fast_forward:", run["conclusion"]
    else:
        icon, state = ":x:", run["conclusion"].replace("_", " ")
    parts = ["%s  *<%s|%s>*" % (icon, run["url"], label)]
    for part in (state, extra, human(duration(run))):
        if part:
            parts.append(part)
    text = "  ·  ".join(parts)
    for failure in failures:
        text += "\n        `%s`" % failure
    return text


def build_payload(tag, repo, sha, subject, actor, server, runs, timed_out, assets, release_url, failures):
    """The Slack message (a legacy attachment holding Block Kit blocks, which is what
    gives it the coloured bar). Pure: no network, no environment."""
    state, _ = classify(runs, timed_out)
    starts = [parse_time(r.get("startedAt")) for r in runs.values() if r.get("startedAt")]
    ends = [parse_time(r.get("updatedAt")) for r in runs.values() if r.get("status") == "completed"]
    elapsed = human((max(ends) - min(starts)).total_seconds()) if starts and ends and max(ends) >= min(starts) else None

    if state == "passed":
        color, headline = GREEN, ":rocket:  *theLB %s is released*" % tag
        summary = "Every job passed: the checks, the Linux builds (amd64, arm, arm64), their SBOMs and checksums, and publishing"
    elif state == "failed":
        color, headline = RED, ":rotating_light:  *theLB %s - the release pipeline failed*" % tag
        summary = "It stopped where shown below, and why"
    else:
        color, headline = AMBER, ":hourglass_flowing_sand:  *theLB %s - the pipeline is still running*" % tag
        summary = "It had not finished when the wait ran out"
    if elapsed:
        summary += "  ·  finished in %s" % elapsed if state != "running" else "  ·  after %s" % elapsed

    lines = []
    for name, label in WORKFLOWS:
        run = runs.get(name)
        extra = None
        if name == "Release" and assets and run and run.get("conclusion") == "success":
            extra = "%d files attached: the Linux binaries, their SHA-256 sums and SBOMs" % assets
        lines.append(line_for(label, run, extra, failures.get(name, [])))

    short = sha[:7]
    context = ["Tag *%s*" % tag, "Commit <%s/%s/commit/%s|`%s`>" % (server, repo, sha, short)]
    if subject:
        context.append(subject if len(subject) <= 80 else subject[:77] + "...")
    if actor:
        context.append("pushed by %s" % actor)

    buttons = []
    if release_url and state == "passed":
        buttons.append({"type": "button", "text": {"type": "plain_text", "text": "Release notes"}, "url": release_url, "style": "primary"})
    buttons.append({"type": "button", "text": {"type": "plain_text", "text": "Workflow runs"},
                    "url": "%s/%s/actions?query=branch%%3A%s" % (server, repo, tag)})

    blocks = [
        {"type": "section", "text": {"type": "mrkdwn", "text": headline + "\n" + summary}},
        {"type": "divider"},
        {"type": "section", "text": {"type": "mrkdwn", "text": "\n".join(lines)}},
        {"type": "context", "elements": [{"type": "mrkdwn", "text": "  ·  ".join(context)}]},
        {"type": "actions", "elements": buttons},
    ]
    fallback = {"passed": "theLB %s released" % tag,
                "failed": "theLB %s: the release pipeline failed" % tag,
                "running": "theLB %s: the release pipeline is still running" % tag}[state]
    return {"text": fallback, "attachments": [{"color": color, "blocks": blocks}]}


def post(url, payload, attempts=3, sleep=time.sleep):
    """POST to the webhook. Returns True on Slack's 'ok'. Never prints the URL."""
    body = json.dumps(payload).encode("utf-8")
    for attempt in range(1, attempts + 1):
        try:
            request = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(request, timeout=20) as response:
                answer = response.read().decode("utf-8", "replace").strip()
            if answer == "ok":
                return True
            print("Slack answered %r" % answer[:80])
            return False  # a message Slack refuses will be refused again
        except urllib.error.HTTPError as err:
            print("Slack answered HTTP %d" % err.code)
            if 400 <= err.code < 500 and err.code != 429:
                return False
        except (urllib.error.URLError, OSError) as err:
            print("could not reach Slack (%s)" % type(err).__name__)
        if attempt < attempts:
            sleep(3 * attempt)
    return False


def write_step_summary(payload, tag):
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    blocks = payload["attachments"][0]["blocks"]
    with open(path, "a", encoding="utf-8") as out:
        out.write("### Slack notification for %s\n\n%s\n" % (tag, blocks[0]["text"]["text"].replace("*", "**")))


def gh_raw(args):
    """Run the gh CLI and return its output as text."""
    done = subprocess.run(["gh"] + list(args), capture_output=True, text=True)
    if done.returncode != 0:
        raise RuntimeError("gh %s failed: %s" % (" ".join(args[:2]), done.stderr.strip()[:200]))
    return done.stdout


def main(env=None, gh=gh_json, sleep=time.sleep, clock=time.monotonic, gh_text=gh_raw):
    env = os.environ if env is None else env
    repo, tag, sha = env.get("REPO", ""), env.get("TAG", ""), env.get("SHA", "")
    if not (repo and tag and sha):
        print("REPO, TAG and SHA are required", file=sys.stderr)
        return 2
    url = env.get("SLACK_WEBHOOK_URL", "")
    dry = env.get("DRY_RUN") == "1"
    if not url and not dry:
        print("SLACK_WEBHOOK_URL is not set (a fork, or the secret is missing): nothing to post.")
        return 0

    names = [n for n, _ in WORKFLOWS]
    runs, timed_out = wait_for_runs(lambda: fetch_runs(gh, repo, sha, tag, env.get("RUN_ID", "")), names,
                                    int(env.get("WAIT_SECONDS", "3000")), int(env.get("POLL_SECONDS", "20")),
                                    sleep=sleep, clock=clock)
    failures = {}
    for name in names:
        run = runs.get(name)
        if run and run.get("status") == "completed" and run["conclusion"] not in OK_CONCLUSIONS + NOT_RUN_CONCLUSIONS:
            failures[name] = failed_steps(gh, repo, run) + ["why: " + line for line in failure_reason(gh_text, repo, run)]

    assets = release_url = subject = None
    try:
        release = gh(["release", "view", tag, "--repo", repo, "--json", "assets,url"])
        assets, release_url = len(release.get("assets", [])), release.get("url")
    except (RuntimeError, AttributeError):
        pass  # no release (yet): the message simply has no release button
    try:
        subject = gh(["api", "repos/%s/commits/%s" % (repo, sha), "--jq", "{message: .commit.message}"])["message"].splitlines()[0]
    except (RuntimeError, IndexError, KeyError, TypeError):
        pass

    payload = build_payload(tag, repo, sha, subject, env.get("ACTOR", ""), env.get("SERVER_URL", "https://github.com"),
                            runs, timed_out, assets, release_url, failures)
    print(payload["text"])
    write_step_summary(payload, tag)
    if dry:
        print(json.dumps(payload, indent=2))
        return 0
    if post(url, payload, sleep=sleep):
        print("Posted to Slack.")
        return 0
    print("The message was not delivered to Slack.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
