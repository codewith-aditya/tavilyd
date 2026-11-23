#!/usr/bin/env python3
"""Write a SearXNG settings copy with outgoing.proxies set.

Usage: render_settings.py <src> <dst> <proxy-url>

The SearXNG image does not ship PyYAML on the system interpreter, so this is a
line-oriented edit that preserves the rest of the file (comments included). The
rendered file carries credentials, hence 0600 in /tmp.
"""

import os
import sys


def yaml_quote(value: str) -> str:
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def main() -> int:
    if len(sys.argv) != 4:
        print("usage: render_settings.py <src> <dst> <proxy-url>", file=sys.stderr)
        return 2
    src, dst, proxy_url = sys.argv[1], sys.argv[2], sys.argv[3]
    with open(src, encoding="utf-8") as fh:
        lines = fh.read().splitlines()

    entry = "  proxies: " + yaml_quote(proxy_url)
    out: list[str] = []
    replaced = False
    in_outgoing = False
    for line in lines:
        if line.strip() == "outgoing:":
            in_outgoing = True
            out.append(line)
            out.append(entry)
            replaced = True
            continue
        if in_outgoing and line and not line.startswith(" "):
            in_outgoing = False
        if in_outgoing and line.lstrip().startswith("proxies:"):
            continue  # drop the old value; the new one was inserted above
        out.append(line)
    if not replaced:
        out.extend(["", "outgoing:", entry])

    tmp = dst + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write("\n".join(out) + "\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, dst)
    print("searxng: outgoing.proxies configured", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
