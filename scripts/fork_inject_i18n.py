#!/usr/bin/env python3
"""Patch the downloaded OpenList frontend bundle for this fork.

Two edits, both to files we do not build and cannot otherwise influence:

1. **StrmSync translations.** The web UI does not render the `help:` text the
   backend sends with each driver field. It uses it only as a flag -- the tips
   row is `<Show when={field.help}>` -- and looks the text itself up in a
   dictionary compiled into the frontend JS, keyed by driver name and JSON tag:

       label    drivers.StrmSync.<json tag>
       tips     drivers.StrmSync.<json tag>-tips
       option   drivers.StrmSync.<json tag>s.<option>

   A key that misses is reported nowhere; the translator capitalises the last
   path segment instead, so an unknown driver shows `LocalMode` as its label
   and `LocalMode-tips` as its help text. `drivers/strm_sync/i18n_test.go`
   checks the JSON against the struct, which is what keeps those keys honest.

   Our driver does not exist upstream and so can never be in that bundle.
   Forking OpenList-Frontend to add three JSON entries would mean maintaining a
   second fork with its own release pipeline; patching the built bundle keeps
   the whole thing inside this repository.

2. **Where a saved storage lands you.** `AddOrEdit.tsx` calls `back()` --
   `navigate(-1)` -- after a successful save, on the assumption that the
   previous history entry is the storage list. Reach the edit page any other
   way (a bookmark, a reload, or the redirect straight after logging in) and
   saving throws you onto whatever *was* there, typically `/@login`, with the
   storage already saved and the session still perfectly valid. This rewrites
   that one call to navigate to the storage list. Unlike the dictionary, it is
   a papercut rather than a correctness fix, so losing the anchor warns instead
   of failing the build.

Chunks are patched **in place**. An earlier version renamed each patched chunk
and rewrote the references, on the theory that a chunk whose content changed
needs a new URL to escape `Cache-Control: max-age=15552000`. That was wrong in
a way worth recording: the importers -- `store-*.js`, `manage-*.js` -- are not
themselves patched, so they keep their names while their contents change, and a
browser holding a cached importer goes on requesting a chunk that no longer
exists. The SPA fallback answers with `index.html`, an ES module import of
`text/html` fails, and the dictionary loader throws. Renaming produced a blank
page precisely when the content changed, which is the only time it was supposed
to do anything. Fixing it properly means cascading new names up the import
graph to something uncached, which for this bundle is most of the dist.

So the names stay, and `server/static/static.go` no longer claims `/assets/` is
immutable -- see the comment there. Patching after the build is what forfeits
the content-addressed filename; the honest response is to stop advertising a
guarantee we broke, not to invent a second hashing scheme on top.

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

# `const { params, back, to } = useRouter()` survives minification as an object
# pattern with those property names intact -- they are property keys, not
# bindings, so the minifier cannot rename them. Only the local names change.
ROUTER_DESTRUCTURE = re.compile(r"\{[^{}]*\bback:(\w+)\b[^{}]*\}=")

# The translation key is written by hand in the frontend source, so it survives
# minification verbatim. `notify.success(t("global.save_success")), back()`.
SAVE_SUCCESS_CALL = "`global.save_success`))"

# The storages editor is not the only page with this shape: the shares and metas
# editors call back() after a successful save too, and each is its own chunk.
# Scoping is the difference between fixing one page and sending every saved
# share to the storage list, so which page a chunk is gets read out of the
# chunk's own edit route rather than assumed. (`/@manage/messenger`, also in the
# storages chunk, has no `/edit/` and does not match.)
EDIT_ROUTE = re.compile(r"`/@manage/(\w+)/edit/")
SECTION = "storages"


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


def patch_save_navigation(text):
    """Send a successful storage save to the storage list, not into history.

    Returns (text, replacements, reason-it-did-nothing).
    """
    sections = set(EDIT_ROUTE.findall(text))
    if sections != {SECTION}:
        found = ", ".join(sorted(sections)) or "none"
        return text, 0, f"not the {SECTION} editor (edit routes: {found})"

    # The back binding is read from one place and the call is rewritten
    # somewhere else in the file; nothing guarantees they are the same
    # function. In today's chunk there is exactly one of each. If a future
    # bundle merges two editors into one chunk there would be two, the first
    # would be picked arbitrarily, and the rewritten call could name a binding
    # that is not in scope at that point -- valid syntax, ReferenceError at
    # runtime. Refuse rather than guess.
    routers = list(ROUTER_DESTRUCTURE.finditer(text))
    if not routers:
        return text, 0, "no useRouter() destructuring binds back:"
    if len(routers) > 1:
        return text, 0, (
            f"{len(routers)} useRouter() destructurings in one chunk; "
            "cannot tell which back() the save handler calls"
        )
    router = routers[0]
    to = re.search(r"\bto:(\w+)\b", router.group(0))
    if to is None:
        return text, 0, "the useRouter() destructuring binds back: but not to:"

    call = re.compile(r"(`global\.save_success`\)\),)" + re.escape(router.group(1)) + r"\(\)")
    replacement = r"\g<1>" + to.group(1) + "(`/@manage/" + SECTION + "`)"
    patched, count = call.subn(replacement, text)
    if count == 0:
        return text, 0, "the save handler no longer calls back() on success"
    return patched, count, None


def inject_dictionary(text, translations):
    """Splice the StrmSync dictionary in front of upstream's Strm one.

    Returns (text, language, reason-it-did-nothing).
    """
    if "StrmSync:{" in text:
        return text, None, "dictionary already patched"
    if ANCHOR not in text:
        return text, None, None  # not a dictionary chunk; nothing to say
    lang = detect_language(text)
    if lang is None:
        return text, None, "language not recognised"
    entries = translations.get(lang)
    if entries is None:
        return text, None, f"no translations for {lang}"
    # Exactly one replacement: a chunk carries a second `Strm:{` inside a map of
    # per-driver alerts, and a StrmSync key spliced in there is dead weight.
    return text.replace(ANCHOR, dictionary_for(entries) + ANCHOR, 1), lang, None


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if len(argv) != 2:
        sys.exit(__doc__)
    dist, i18n_path = pathlib.Path(argv[0]), pathlib.Path(argv[1])

    translations = json.loads(i18n_path.read_text(encoding="utf-8"))
    translations.pop("_comment", None)

    # Nothing is written until every chunk has been considered. An earlier
    # version wrote as it went and then bailed out on a missing anchor, which
    # left the navigation patch on disk under its original name and a rerun
    # reporting that it had not been applied.
    planned, dictionaries, navigation, skipped = {}, [], [], []
    for path in sorted(dist.rglob("*.js")):
        if not path.is_file():
            continue
        original = path.read_text(encoding="utf-8", errors="surrogateescape")
        text = original

        # Navigation first. The scope check reads every `/@manage/<x>/edit/` in
        # the chunk, and a translation containing one would otherwise be able
        # to veto the patch.
        if SAVE_SUCCESS_CALL in text:
            text, count, reason = patch_save_navigation(text)
            if count:
                navigation.append(path.name)
            else:
                skipped.append(f"{path.name}: save navigation untouched -- {reason}")

        text, lang, reason = inject_dictionary(text, translations)
        if lang:
            dictionaries.append(f"{path.name} ({lang})")
        elif reason:
            skipped.append(f"{path.name}: {reason}")

        if text != original:
            planned[path] = text

    for line in dictionaries:
        print(f"  dictionary {line}")
    for line in navigation:
        print(f"  navigation {line}")
    for line in skipped:
        print(f"  skipped    {line}")

    if not dictionaries:
        print(
            f"error: no chunk under {dist} took the StrmSync dictionary.\n"
            "Either the bundle no longer looks the way this script expects, or it\n"
            "has already been patched -- check the skip lines above. Nothing has\n"
            "been written; the StrmSync form would otherwise ship with\n"
            "untranslated placeholders.",
            file=sys.stderr,
        )
        return 1
    if not navigation:
        print(
            "warning: no chunk was patched to send a saved storage to the storage\n"
            "list, so saving one may still drop you wherever browser history last\n"
            "was. Check whether upstream fixed AddOrEdit.tsx and drop this patch.",
            file=sys.stderr,
        )

    for path, text in planned.items():
        path.write_text(text, encoding="utf-8", errors="surrogateescape")
    print(f"patched {len(planned)} chunk(s) in place")
    return 0


if __name__ == "__main__":
    sys.exit(main())
