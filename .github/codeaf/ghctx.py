#!/usr/bin/env python3
"""GitHub glue for the CodeAF workflow. Stdlib only.

  ghctx.py gate    decide whether this event may run CodeAF; write step outputs
                   (run, reason, mode, number, branch, base) to $GITHUB_OUTPUT
  ghctx.py brief   write the task brief for `codeaf do` to $BRIEF_PATH
  ghctx.py report  print the Markdown run report (cost table etc.) to stdout

Trust model: the `codeaf` label can only be added by people with triage access
or above, and follow-up commands must come from OWNER/MEMBER/COLLABORATOR.
Text from the issue/PR is treated as the task, never as shell input.
"""

import json
import os
import sys
import urllib.request
from collections import defaultdict

LABEL = os.environ.get("CODEAF_LABEL", "codeaf")
COMMAND = "/codeaf"
TRUSTED = {"OWNER", "MEMBER", "COLLABORATOR"}
REPO = os.environ["GITHUB_REPOSITORY"]
API = os.environ.get("GITHUB_API_URL", "https://api.github.com")
HERE = os.path.dirname(os.path.abspath(__file__))


def api(path):
    request = urllib.request.Request(
        API + path,
        headers={"Authorization": "Bearer " + os.environ["GITHUB_TOKEN"],
                 "Accept": "application/vnd.github+json"})
    with urllib.request.urlopen(request) as response:
        return json.load(response)


def event():
    with open(os.environ["GITHUB_EVENT_PATH"]) as f:
        return json.load(f)


def labels_of(item):
    return {label["name"] for label in item.get("labels", [])}


def output(**values):
    with open(os.environ["GITHUB_OUTPUT"], "a") as f:
        for key, value in values.items():
            f.write("%s=%s\n" % (key, value))
    print(json.dumps(values))


def gate():
    name = os.environ["GITHUB_EVENT_NAME"]
    ev = event()
    skip = lambda reason: output(run="false", reason=reason)

    if name == "issues":
        issue = ev["issue"]
        if ev["label"]["name"] != LABEL:
            return skip("label is not " + LABEL)
        if "pull_request" in issue:
            return skip("labelled a PR; PRs act on /codeaf commands")
        return output(run="true", reason="issue labelled", mode="issue", number=issue["number"],
                      branch="codeaf/issue-%d" % issue["number"], base=ev["repository"]["default_branch"])

    if name == "issue_comment":
        issue, comment = ev["issue"], ev["comment"]
        body = comment["body"].strip()
    elif name == "pull_request_review":
        issue, comment = ev["pull_request"], ev["review"]
        body = (comment.get("body") or "").strip()
        if comment.get("state") == "changes_requested" and not body.startswith(COMMAND):
            body = COMMAND + " " + body
    else:
        return skip("unhandled event " + name)

    if LABEL not in labels_of(issue):
        return skip("item is not labelled " + LABEL)
    if not body.startswith(COMMAND):
        return skip("comment is not a %s command" % COMMAND)
    if comment.get("author_association") not in TRUSTED:
        return skip("author %s is %s, not trusted" % (comment["user"]["login"], comment.get("author_association")))
    if comment["user"]["type"] == "Bot":
        return skip("bot comment")

    if name == "issue_comment" and "pull_request" not in issue:
        return output(run="true", reason="command on issue", mode="issue", number=issue["number"],
                      branch="codeaf/issue-%d" % issue["number"], base=ev["repository"]["default_branch"])

    pr = api("/repos/%s/pulls/%d" % (REPO, issue["number"]))
    if pr["state"] != "open":
        return skip("PR is not open")
    if pr["head"]["repo"] is None or pr["head"]["repo"]["full_name"] != REPO:
        return skip("PR head is in a fork; CodeAF only pushes to branches in " + REPO)
    return output(run="true", reason="command on PR", mode="pr", number=pr["number"],
                  branch=pr["head"]["ref"], base=pr["base"]["ref"])


def thread(number, limit=30):
    comments = api("/repos/%s/issues/%d/comments?per_page=100" % (REPO, number))
    lines = []
    for c in comments[-limit:]:
        if c["user"]["type"] == "Bot" and "<!-- codeaf-report -->" in (c.get("body") or ""):
            body = "(CodeAF run report omitted)"
        else:
            body = c["body"]
        lines.append("**@%s** (%s):\n%s\n" % (c["user"]["login"], c.get("author_association"), body))
    return "\n".join(lines) or "(no comments)"


def brief():
    mode, number = os.environ["MODE"], int(os.environ["NUMBER"])
    ev = event()
    with open(os.path.join(HERE, "instructions.md")) as f:
        parts = [f.read()]

    if mode == "issue":
        issue = api("/repos/%s/issues/%d" % (REPO, number))
        parts.append("# Task: issue #%d — %s\n\n%s" % (number, issue["title"], issue.get("body") or ""))
        if issue.get("comments"):
            parts.append("## Discussion on the issue\n\n" + thread(number))
    else:
        pr = api("/repos/%s/pulls/%d" % (REPO, number))
        parts.append("# Follow-up on pull request #%d — %s\n\nYou are continuing work on an open pull request. "
                     "The working tree is its head branch `%s`. Address the request below; "
                     "do not redo work that is already on the branch.\n\n## PR description\n\n%s"
                     % (number, pr["title"], pr["head"]["ref"], pr.get("body") or ""))
        reviews = api("/repos/%s/pulls/%d/comments?per_page=100" % (REPO, number))
        if reviews:
            parts.append("## Inline review comments\n\n" + "\n".join(
                "- `%s` line %s, @%s: %s" % (c["path"], c.get("line") or c.get("original_line"),
                                             c["user"]["login"], c["body"]) for c in reviews[-40:]))
        parts.append("## Conversation\n\n" + thread(number))

    if "comment" in ev or "review" in ev:
        trigger = ev.get("comment") or ev.get("review")
        text = (trigger.get("body") or "").strip()
        if text.startswith(COMMAND):
            text = text[len(COMMAND):].strip()
        parts.append("## The request you are acting on now (from @%s)\n\n%s"
                     % (trigger["user"]["login"], text or "(no text: continue / fix what the reviews ask)"))

    with open(os.environ["BRIEF_PATH"], "w") as f:
        f.write("\n\n".join(parts) + "\n")
    print(open(os.environ["BRIEF_PATH"]).read())


def money(x):
    return "$%.4f" % x


def report():
    result = {}
    try:
        with open(os.environ["RESULT_PATH"]) as f:
            result = json.load(f)
    except (OSError, ValueError):
        pass
    rows = defaultdict(lambda: {"calls": 0, "in": 0, "cached": 0, "out": 0, "cost": 0.0, "errors": 0})
    try:
        with open(os.environ["LEDGER_PATH"]) as f:
            for line in f:
                r = json.loads(line)
                row = rows[r["model"]]
                row["calls"] += 1
                row["in"] += int(r.get("prompt_tokens") or 0)
                row["cached"] += int(r.get("cached_tokens") or 0)
                row["out"] += int(r.get("completion_tokens") or 0)
                row["cost"] += float(r.get("cost_usd") or 0)
    except OSError:
        pass
    with open(os.path.join(HERE, "models.json")) as f:
        prices = {m["id"]: m for m in json.load(f)["models"]}
    total = sum(r["cost"] for r in rows.values())
    day_before = float(os.environ.get("DAY_SPENT_BEFORE") or 0)

    out = ["<!-- codeaf-report -->", "### CodeAF run — %s" % os.environ.get("OUTCOME", "finished"), ""]
    out.append("| | |\n|---|---|")
    out.append("| stop / ok | `%s` / `%s` |" % (result.get("stop", "?"), result.get("ok", "?")))
    if result.get("verdict"):
        out.append("| verdict | `%s` |" % result["verdict"])
    if result.get("error"):
        out.append("| error | %s |" % str(result["error"]).replace("\n", " ")[:300])
    out.append("| seats | worker `%s`, planner `%s`, checker `%s` (escalation panel: %s) |" % (
        result.get("model", "?"), result.get("plan_model", "?"), result.get("check_model", "?"),
        " → ".join("`%s`" % m for m in prices)))
    out.append("| CodeAF's own spend figure | %s (plan estimate %s) |" % (
        money(float(result.get("spend_usd") or 0)), money(float(result.get("est_usd") or 0))))
    out.append("| metered by proxy | **%s** this run · %s today (cap $%s/run, $%s/day) |" % (
        money(total), money(day_before + total), os.environ.get("RUN_CAP", "5"), os.environ.get("DAY_CAP", "20")))
    out.append("| files changed | %s |" % os.environ.get("CHANGED", "?"))
    out.append("")
    if rows:
        out.append("| model | $/M in · cached · out | calls | input tok | cached tok | output tok | cost |")
        out.append("|---|---|---:|---:|---:|---:|---:|")
        for model, r in sorted(rows.items(), key=lambda kv: -kv[1]["cost"]):
            p = prices.get(model, {})
            out.append("| `%s` | %s · %s · %s | %d | %d | %d | %d | %s |" % (
                model, p.get("input"), p.get("cache_read"), p.get("output"),
                r["calls"], r["in"], r["cached"], r["out"], money(r["cost"])))
        out.append("")
    if result.get("answer"):
        out.append("<details><summary>CodeAF's summary</summary>\n\n%s\n\n</details>\n" % str(result["answer"])[:6000])
    out.append("[Workflow run](%s/%s/actions/runs/%s) · reply `/codeaf <instructions>` on a `%s`-labelled "
               "issue/PR to run again." % (os.environ.get("GITHUB_SERVER_URL", "https://github.com"), REPO,
                                         os.environ.get("GITHUB_RUN_ID", ""), LABEL))
    print("\n".join(out))


if __name__ == "__main__":
    {"gate": gate, "brief": brief, "report": report}[sys.argv[1]]()
