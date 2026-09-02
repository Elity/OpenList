#!/usr/bin/env python3
"""Splice the StrmSync driver translations into a downloaded frontend bundle.

OpenList's web UI does not render the `help:` text the backend sends with each
driver field. It uses it only as a flag -- the tips row is
`<Show when={field.help}>` -- and looks the text itself up in a dictionary
compiled into the frontend JS, keyed by driver name and JSON tag:

    label    drivers.StrmSync.<json tag>
    tips     drivers.StrmSync.<json tag>-tips
    option   drivers.StrmSync.<json tag>s.<option>

A key that misses is reported nowhere; the translator capitalises the last path
segment instead, so an unknown driver shows `LocalMode` as its label and
`LocalMode-tips` as its help text. `drivers/strm_sync/i18n_test.go` checks the
JSON against the struct, which is what keeps those keys honest.

Our driver does not exist upstream and so can never be in that bundle. Forking
OpenList-Frontend to add three JSON entries would mean maintaining a second fork
with its own release pipeline; patching the built bundle keeps the whole thing
inside this repository.

The insertion point is the `Strm:{` that opens the upstream Strm driver's
dictionary in each per-language chunk. That anchor is stable because it comes
from a JSON source file rather than from minifier output, and every occurrence
is checked: if the bundle ever stops matching, this exits non-zero and the image
build fails rather than quietly shipping the placeholders again.

Usage: fork_inject_i18n.py <dist-dir> <i18n-json>
"""

import json
import pathlib
import re
import sys

# Upstream's Strm dictionary opens each per-language driver section. Inserting
# immediately before it keeps us inside the same object without having to parse
# minified JavaScript. "StrmSync:{" does not itself contain "Strm:{", so a
# second run would not nest.
ANCHOR = "Strm:{"

# Which language a chunk holds, decided by a string upstream already translated.
# Simplified and traditional Chinese differ on this character, and the English
# bundle leaves the value equal to the key.
LANG_PROBES = [
    ("zh-CN", re.compile(r"PathPrefix:`路径前缀`")),
    ("zh-TW", re.compile(r"PathPrefix:`路徑前綴`")),
    ("en", re.compile(r"PathPrefix:`PathPrefix`")),
]


def js_literal(value):
    """Render a Python value as the backtick-quoted form the bundle uses."""
    if isinstance(value, dict):
        return "{" + ",".join(f"{json.dumps(k)}:{js_literal(v)}" for k, v in value.items()) + "}"
    escaped = value.replace("\\", "\\\\").replace("`", "\\`").replace("$", "\\$")
    return f"`{escaped}`"


def dictionary_for(entries):
    body = ",".join(f"{json.dumps(k)}:{js_literal(v)}" for k, v in entries.items())
    return "StrmSync:{" + body + "},"


def detect_language(text):
    for lang, probe in LANG_PROBES:
        if probe.search(text):
            return lang
    return None


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    dist, i18n_path = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])

    translations = json.loads(i18n_path.read_text(encoding="utf-8"))
    translations.pop("_comment", None)

    patched, skipped = [], []
    for path in sorted(dist.rglob("*.js")):
        if not path.is_file():
            continue
        text = path.read_text(encoding="utf-8", errors="surrogateescape")
        if ANCHOR not in text:
            continue
        if "StrmSync:{" in text:
            skipped.append(f"{path.name}: already patched")
            continue

        lang = detect_language(text)
        if lang is None:
            skipped.append(f"{path.name}: language not recognised")
            continue
        entries = translations.get(lang)
        if entries is None:
            skipped.append(f"{path.name}: no translations for {lang}")
            continue

        # Exactly one replacement: the anchor opens one object per chunk.
        text = text.replace(ANCHOR, dictionary_for(entries) + ANCHOR, 1)
        path.write_text(text, encoding="utf-8", errors="surrogateescape")
        patched.append(f"{path.name} ({lang})")

    for line in patched:
        print(f"  patched {line}")
    for line in skipped:
        print(f"  skipped {line}")

    if not patched:
        print(
            f"error: no chunk under {dist} contained {ANCHOR!r}.\n"
            "The frontend bundle no longer looks the way this script expects, so the\n"
            "StrmSync form would ship with untranslated placeholders. Re-check the\n"
            "layout of the driver dictionary in the dist and update ANCHOR.",
            file=sys.stderr,
        )
        return 1
    print(f"injected StrmSync translations into {len(patched)} chunk(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
