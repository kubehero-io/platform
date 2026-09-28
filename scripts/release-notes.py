#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) KubeHero contributors
"""Print the CHANGELOG.md section for a version as a GitHub Release body.

    scripts/release-notes.py v0.3.0 [CHANGELOG.md]

Takes the "## <version> ..." section up to the next "## " heading and
joins hard-wrapped lines: GitHub renders every newline in a release body
as <br>, while the changelog is wrapped for reading in the repo.
Headings, list items, tables, quotes and fenced code keep their lines.
Exits 1 when the version has no section.
"""
import re
import sys


def section(text: str, version: str) -> list[str]:
    lines, on = [], False
    for line in text.split("\n"):
        if line.startswith(f"## {version} ") or line.rstrip() == f"## {version}":
            on = True
            continue
        if on and line.startswith("## "):
            break
        if on:
            lines.append(line)
    return lines


def unwrap(lines: list[str]) -> str:
    out: list[str] = []
    code = False
    for raw in lines:
        line = raw.rstrip()
        t = line.lstrip()
        if t.startswith("```"):
            code = not code
            out.append(line)
            continue
        starts = code or not t or t.startswith(("#", "- ", "* ", "|", ">")) or re.match(r"\d+\. ", t)
        if not starts and out and out[-1].strip() and not out[-1].lstrip().startswith(("#", "```")):
            out[-1] += " " + t
        else:
            out.append(line)
    return "\n".join(out).strip() + "\n"


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    path = sys.argv[2] if len(sys.argv) > 2 else "CHANGELOG.md"
    with open(path, encoding="utf-8") as f:
        lines = section(f.read(), sys.argv[1])
    if not any(line.strip() for line in lines):
        print(f"no {sys.argv[1]} section in {path}", file=sys.stderr)
        return 1
    sys.stdout.write(unwrap(lines))
    return 0


if __name__ == "__main__":
    sys.exit(main())
