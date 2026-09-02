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

Every chunk touched is renamed and its importers repointed. Vite hashes those
filenames over *upstream's* build, and the dist is served with
`Cache-Control: max-age=15552000` and no validator, so a patch applied
afterwards would otherwise ship under a URL browsers already have cached.

Usage: fork_inject_i18n.py <dist-dir> <i18n-json>
"""

import hashlib
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
    router = ROUTER_DESTRUCTURE.search(text)
    if router is None:
        return text, 0, "no useRouter() destructuring binds back:"
    to = re.search(r"\bto:(\w+)\b", router.group(0))
    if to is None:
        return text, 0, "the useRouter() destructuring binds back: but not to:"

    call = re.compile(r"(`global\.save_success`\)\),)" + re.escape(router.group(1)) + r"\(\)")
    replacement = r"\g<1>" + to.group(1) + "(`/@manage/" + SECTION + "`)"
    patched, count = call.subn(replacement, text)
    if count == 0:
        return text, 0, "the save handler no longer calls back() on success"
    return patched, count, None


def rehash(path):
    """Give a patched chunk a name that reflects what is now inside it.

    Not cosmetic: correcting the dictionary keys from `LocalMode` to
    `localMode` changed only the case of leading letters, which left every
    chunk byte-for-byte the same length. Without a new URL the fix reached
    nobody who had already opened the page.

    Returns the new basename; the caller rewrites the references.
    """
    digest = hashlib.sha256(path.read_bytes()).hexdigest()[:8]
    return f"{path.stem}.sy{digest}{path.suffix}"


def repoint(dist, renames):
    """Point every reference in the dist at the renamed chunks.

    A chunk basename is a long unique token, so a plain string replacement is
    safe and catches the SystemJS loader's legacy references as well as the
    module graph's. Sourcemap filenames embed the chunk name, so they follow
    from the same substitution.
    """
    edits = {old: 0 for old in renames}
    for path in sorted(dist.rglob("*")):
        if not path.is_file():
            continue
        try:
            text = path.read_text(encoding="utf-8", errors="surrogateescape")
        except (UnicodeDecodeError, OSError):
            continue
        patched = text
        for old, new in renames.items():
            if old in patched:
                edits[old] += patched.count(old)
                patched = patched.replace(old, new)
        if patched != text:
            path.write_text(patched, encoding="utf-8", errors="surrogateescape")

    for old, count in edits.items():
        if count == 0:
            raise SystemExit(
                f"error: nothing in the dist refers to {old!r}.\n"
                "The chunk was renamed to bust the cache, but no importer was "
                "found to point at the new name, so the app would 404 on it."
            )
    return edits


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if len(argv) != 2:
        sys.exit(__doc__)
    dist, i18n_path = pathlib.Path(argv[0]), pathlib.Path(argv[1])

    translations = json.loads(i18n_path.read_text(encoding="utf-8"))
    translations.pop("_comment", None)

    dictionaries, navigation, skipped, touched = [], [], [], []
    for path in sorted(dist.rglob("*.js")):
        if not path.is_file():
            continue
        original = path.read_text(encoding="utf-8", errors="surrogateescape")
        text = original

        if "StrmSync:{" in text:
            skipped.append(f"{path.name}: dictionary already patched")
        elif ANCHOR in text:
            lang = detect_language(text)
            if lang is None:
                skipped.append(f"{path.name}: language not recognised")
            elif translations.get(lang) is None:
                skipped.append(f"{path.name}: no translations for {lang}")
            else:
                # Exactly one replacement: the anchor opens one object per chunk.
                text = text.replace(ANCHOR, dictionary_for(translations[lang]) + ANCHOR, 1)
                dictionaries.append(f"{path.name} ({lang})")

        if SAVE_SUCCESS_CALL in text:
            text, count, reason = patch_save_navigation(text)
            if count:
                navigation.append(path.name)
            else:
                skipped.append(f"{path.name}: save navigation untouched -- {reason}")

        if text != original:
            path.write_text(text, encoding="utf-8", errors="surrogateescape")
            touched.append(path)

    for line in dictionaries:
        print(f"  dictionary {line}")
    for line in navigation:
        print(f"  navigation {line}")
    for line in skipped:
        print(f"  skipped    {line}")

    if not dictionaries:
        print(
            f"error: no chunk under {dist} contained {ANCHOR!r}.\n"
            "The frontend bundle no longer looks the way this script expects, so the\n"
            "StrmSync form would ship with untranslated placeholders. Re-check the\n"
            "layout of the driver dictionary in the dist and update ANCHOR.",
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

    renames = {}
    for path in touched:
        renamed = rehash(path)
        path.rename(path.with_name(renamed))
        renames[path.name] = renamed
    edits = repoint(dist, renames)

    print(f"repointed {sum(edits.values())} reference(s) to {len(renames)} renamed chunk(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
