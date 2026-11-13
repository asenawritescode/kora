#!/usr/bin/env python3
"""Release the Engine through pull requests, never by pushing to main.

  scripts/release.py prepare vX.Y.Z [--ticket N]   open a PR that bumps VERSION and CHANGELOG.md
  scripts/release.py tag vX.Y.Z                    tag the merged commit once CI is green on it

Pushing the tag starts the Release workflow, which publishes the binaries, the GitHub release and the
Docker image (X.Y.Z, vX.Y.Z and latest). Environment: KORA_RELEASE_REMOTE (default kora-upstream), whose URL
also names the GitHub repository for every gh call (override with KORA_RELEASE_REPO); the clone's origin is not used.
"""

from __future__ import annotations

import argparse
import datetime
import json
import os
import re
import subprocess
import sys
from pathlib import Path

REMOTE = os.environ.get("KORA_RELEASE_REMOTE", "kora-upstream")
MAIN = f"{REMOTE}/main"
REQUIRED_JOBS = ("Go", "Lint")  # advisory jobs (continue-on-error) do not gate a release
TAG_PATTERN = re.compile(r"^v(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?$")
SECTIONS = (
    ("Features", ("feat",)),
    ("Fixes", ("fix",)),
    ("Security", ("security",)),
    ("Improvements", ("refactor", "perf")),
    ("Documentation", ("docs",)),
)


class ReleaseError(Exception):
    """A precondition failed; the message says what to fix."""


def parse_tag(tag: str) -> str:
    """Return the version for a tag such as v1.2.3 or v1.0.0-alpha.2."""
    if not TAG_PATTERN.match(tag):
        raise ReleaseError(f"{tag!r} is not a version tag like v1.2.3 or v1.2.3-alpha.1")
    return tag[1:]


def commit_type(subject: str) -> str:
    """The conventional-commit type of a subject, lowercased, or '' when it has none."""
    match = re.match(r"^([A-Za-z]+)(\([^)]*\))?!?:\s", subject)
    return match.group(1).lower() if match else ""


def changelog_entry(tag: str, today: str, subjects: list[str], previous: str, repo: str) -> str:
    """The markdown entry for a release, grouping subjects by commit type."""
    lines = [f"## {tag} — {today}"]
    for title, types in SECTIONS:
        picked = [re.sub(r" \(#\d+\)$", "", s) for s in subjects if commit_type(s) in types]
        if picked:
            lines += ["", f"### {title}"] + [f"- {s}" for s in picked]
    if previous:
        lines += ["", f"**Full diff:** https://github.com/{repo}/compare/{previous}...{tag}"]
    return "\n".join(lines) + "\n"


def prepend(entry: str, existing: str) -> str:
    return entry + ("\n\n" + existing if existing.strip() else "")


def ci_gate(jobs: list[dict]) -> str:
    """'' when every required job succeeded, else why the release must wait."""
    by_name = {job["name"]: job.get("conclusion") for job in jobs}
    for name in REQUIRED_JOBS:
        if name not in by_name:
            return f"CI has no {name!r} job on this commit yet"
        if by_name[name] != "success":
            return f"CI job {name!r} is {by_name[name] or 'still running'}"
    return ""


def run(*args: str, cwd: Path | None = None) -> str:
    done = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if done.returncode != 0:
        raise ReleaseError(f"{' '.join(args)} failed: {(done.stderr or done.stdout).strip()}")
    return done.stdout.strip()


def repo_slug() -> str:
    """owner/name of the GitHub repository behind the release remote."""
    explicit = os.environ.get("KORA_RELEASE_REPO")
    if explicit:
        return explicit
    url = run("git", "remote", "get-url", REMOTE)
    match = re.search(r"github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$", url)
    if not match:
        raise ReleaseError(f"cannot find a GitHub repository in {REMOTE}'s URL {url!r}; set KORA_RELEASE_REPO")
    return match.group(1)


def tag_exists(tag: str) -> bool:
    return bool(run("git", "tag", "-l", tag)) or bool(run("git", "ls-remote", "--tags", REMOTE, tag))


def prepare_release(tag: str, ticket: str, today: str) -> None:
    version = parse_tag(tag)
    repo = repo_slug()
    if run("git", "status", "--porcelain"):
        raise ReleaseError("the working tree is not clean")
    run("git", "fetch", "-q", REMOTE)
    if run("git", "rev-parse", "HEAD") != run("git", "rev-parse", MAIN):
        raise ReleaseError(f"HEAD is not {MAIN}; release from a checkout of the current main")
    if tag_exists(tag):
        raise ReleaseError(f"{tag} already exists")
    run("go", "build", "-o", os.devnull, "./cmd/kora")
    try:
        previous = run("git", "describe", "--tags", "--abbrev=0", "HEAD")
    except ReleaseError:
        previous = ""
    range_spec = f"{previous}..HEAD" if previous else "HEAD"
    subjects = run("git", "log", "--no-merges", "--format=%s", range_spec).splitlines()
    run("git", "checkout", "-q", "-b", f"release/{tag}")
    Path("VERSION").write_text(version + "\n")
    changelog = Path("CHANGELOG.md")
    existing = changelog.read_text() if changelog.exists() else ""
    changelog.write_text(prepend(changelog_entry(tag, today, subjects, previous, repo), existing))
    run("git", "add", "VERSION", "CHANGELOG.md")
    run("git", "commit", "-q", "-m", f"release: {tag}")
    run("git", "push", "-q", "-u", REMOTE, f"release/{tag}")
    title = f"#{ticket}: release: {tag}" if ticket else f"release: {tag}"
    body = (f"Closes #{ticket}\n\n" if ticket else "") + (
        f"VERSION to {version} and a CHANGELOG entry from {len(subjects)} commits since {previous or 'the start'}.\n"
        f"After this merges and CI is green on main, run: scripts/release.py tag {tag}")
    url = run("gh", "pr", "create", "--repo", repo, "--base", "main", "--head", f"release/{tag}", "--title", title, "--body", body)
    print(f"opened {url}\nafter it merges: scripts/release.py tag {tag}")


def tag_release(tag_name: str) -> None:
    version = parse_tag(tag_name)
    repo = repo_slug()
    run("git", "fetch", "-q", REMOTE)
    sha = run("git", "rev-parse", MAIN)
    if run("git", "show", f"{sha}:VERSION") != version:
        raise ReleaseError(f"VERSION on {MAIN} is not {version}; merge the release PR first")
    if f"## {tag_name} " not in run("git", "show", f"{sha}:CHANGELOG.md"):
        raise ReleaseError(f"CHANGELOG.md on {MAIN} has no {tag_name} entry")
    if tag_exists(tag_name):
        raise ReleaseError(f"{tag_name} already exists")
    runs = run("gh", "run", "list", "--repo", repo, "--branch", "main", "--commit", sha, "--workflow", "ci.yml",
               "--json", "databaseId,status", "--jq",
               "if length == 0 then \"\" else .[0] | \"\\(.databaseId) \\(.status)\" end")
    if not runs or runs.split()[1] != "completed":
        raise ReleaseError(f"CI on {sha[:8]} has not finished: {runs or 'no run found'}")
    jobs_json = run("gh", "api", f"repos/{repo}/actions/runs/{runs.split()[0]}/jobs",
                    "--jq", "[.jobs[] | {name, conclusion}]")
    why = ci_gate(json.loads(jobs_json))
    if why:
        raise ReleaseError(f"{why}; not tagging {sha[:8]}")
    run("git", "tag", "-a", tag_name, "-m", tag_name, sha)
    run("git", "push", REMOTE, tag_name)
    print(f"tagged {sha[:8]} as {tag_name}; the Release workflow now publishes the binaries and the Docker image")


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    prep = sub.add_parser("prepare")
    prep.add_argument("tag")
    prep.add_argument("--ticket", default="")
    sub.add_parser("tag").add_argument("tag")
    args = parser.parse_args(argv)
    try:
        if args.command == "prepare":
            prepare_release(args.tag, args.ticket, datetime.date.today().isoformat())
        else:
            tag_release(args.tag)
    except ReleaseError as err:
        print(f"release: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
