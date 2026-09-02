#!/usr/bin/env python3
"""Tests for fork_inject_i18n.py. Run: python3 scripts/fork_inject_i18n_test.py

The fixtures are lifted verbatim from a shipped OpenList-Frontend bundle rather
than written by hand, because both patches this script applies are anchored on
minifier output and a hand-written approximation would happily pass while the
real bundle did not match.
"""

import contextlib
import io
import json
import pathlib
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import fork_inject_i18n as inject

# Verbatim from assets/AddOrEdit-ns-WA8MG.js. `i` is useRouter().back and `o`
# is useRouter().to; on a successful save the bundle calls `i()`, i.e.
# navigate(-1). That is the bug: browser history back is not "the storage
# list". Land on the edit page from the login screen -- or just reload it --
# and saving throws you onto /@login with the storage already saved.
ADD_OR_EDIT = (
    "var z=()=>{let t=e(),{params:r,back:i,to:o}=k(),{id:u}=r,"
    "[f,p]=S(()=>d.get(`/admin/driver/list`),!0),[m,h]=T({});"
    "let e=await q();n(e,()=>{x.success(t(`global.save_success`)),i()},"
    "(t,n)=>{e.data.id&&o(`/@manage/storages/edit/${e.data.id}`)})};"
)

# One per-language i18n chunk, trimmed to the shape the injector cares about.
ENTRY_CHUNK = (
    "var x={drivers:{Local:{},Strm:{PathPrefix:`路径前缀`,"
    '"PathPrefix-tips":`路径前缀`,paths:`路径`}},'
    "global:{save_success:`保存成功`}};export{x};"
)

# src/pages/manage/shares/AddOrEdit.tsx has the same save_success + back()
# shape, in its own chunk. Patching it would send every saved share to the
# storage list. src/pages/manage/metas/AddOrEdit.tsx has it too but carries no
# /@manage route at all, so it falls out of scope on the same check.
SHARES_EDITOR = (
    "var z=()=>{let t=e(),{params:r,back:i,to:o}=k();"
    "n(e,()=>{x.success(t(`global.save_success`)),i()},"
    "(t,n)=>{e.data.id&&o(`/@manage/shares/edit/${e.data.id}`)})};"
)


class SaveNavigationTest(unittest.TestCase):
    def test_the_shipped_bundle_navigates_back_after_a_successful_save(self):
        """The bug, stated as an assertion about the real bundle."""
        self.assertIn("`global.save_success`)),i()", ADD_OR_EDIT)

    def test_the_patch_sends_a_successful_save_to_the_storage_list(self):
        patched, count, reason = inject.patch_save_navigation(ADD_OR_EDIT)
        self.assertIsNone(reason)
        self.assertEqual(count, 1)
        self.assertNotIn("`global.save_success`)),i()", patched)
        self.assertIn(
            "`global.save_success`)),o(`/@manage/storages`)", patched
        )

    def test_the_patch_leaves_the_rest_of_the_chunk_alone(self):
        patched, _, _ = inject.patch_save_navigation(ADD_OR_EDIT)
        # The failure branch still routes to the newly created storage, and the
        # router destructuring is untouched.
        self.assertIn("{params:r,back:i,to:o}=k()", patched)
        self.assertIn("o(`/@manage/storages/edit/${e.data.id}`)", patched)
        self.assertEqual(len(patched) - len(ADD_OR_EDIT), len("o(`/@manage/storages`)") - len("i()"))

    def test_the_shares_editor_is_left_alone(self):
        patched, count, reason = inject.patch_save_navigation(SHARES_EDITOR)
        self.assertEqual(count, 0)
        self.assertEqual(patched, SHARES_EDITOR)
        self.assertIn("shares", reason)

    def test_a_chunk_naming_two_sections_is_too_ambiguous_to_patch(self):
        text = ADD_OR_EDIT + SHARES_EDITOR
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertEqual(count, 0)
        self.assertEqual(patched, text)
        self.assertIsNotNone(reason)

    def test_patching_twice_is_a_no_op(self):
        once, _, _ = inject.patch_save_navigation(ADD_OR_EDIT)
        twice, count, _ = inject.patch_save_navigation(once)
        self.assertEqual(count, 0)
        self.assertEqual(twice, once)

    def test_a_chunk_without_the_router_is_left_alone(self):
        text = "x.success(t(`global.save_success`)),i();o(`/@manage/storages/edit/${x}`)"
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertEqual(count, 0)
        self.assertEqual(patched, text)
        self.assertIn("back:", reason)

    def test_a_router_that_does_not_bind_to_is_left_alone(self):
        text = (
            "{params:r,back:i}=k();x.success(t(`global.save_success`)),i();"
            "q(`/@manage/storages/edit/${x}`)"
        )
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertEqual(count, 0)
        self.assertEqual(patched, text)
        self.assertIn("to:", reason)

    def test_the_translation_of_save_success_is_not_mistaken_for_a_call(self):
        """`global.save_success` is also a dictionary key; only the call site
        has the template-literal form the pattern requires."""
        text = (
            "{params:r,back:i,to:o}=k();var d={global:{save_success:`保存成功`}};"
            "o(`/@manage/storages/edit/${x}`)"
        )
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertEqual(count, 0)
        self.assertEqual(patched, text)
        self.assertIn("back()", reason)

    def test_the_dictionary_chunk_is_out_of_scope_entirely(self):
        patched, count, reason = inject.patch_save_navigation(ENTRY_CHUNK)
        self.assertEqual(count, 0)
        self.assertEqual(patched, ENTRY_CHUNK)
        # Assert *why*: without this the test passes under a build that has
        # lost the section check, because some later guard rejects it anyway.
        self.assertIn("not the storages editor", reason)

    def test_a_manage_route_without_edit_does_not_widen_the_scope(self):
        """`/@manage/messenger` rides in the storages chunk and has no
        `/edit/`. Only edit routes may decide which editor a chunk is, or the
        storages patch silently stops applying to the storages chunk."""
        text = ADD_OR_EDIT + "u(`/@manage/messenger`);"
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertIsNone(reason)
        self.assertEqual(count, 1)
        self.assertIn("`global.save_success`)),o(`/@manage/storages`)", patched)

    def test_only_the_back_binding_is_rewritten_not_any_call(self):
        """The call rewritten has to be back(). Upstream reordering the handler
        so some other nullary call follows save_success must abort the patch,
        not silently redirect that call to the storage list."""
        text = ADD_OR_EDIT.replace("`global.save_success`)),i()", "`global.save_success`)),p()")
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertEqual(count, 0)
        self.assertEqual(patched, text)
        self.assertIn("back()", reason)

    def test_a_to_binding_from_a_different_object_is_not_borrowed(self):
        """back: and to: must come out of the same destructuring. Pairing a
        back from one object with a to from another rewrites the call to a
        binding that need not be in scope at the call site."""
        text = (
            "let {params:r,back:i}=k(),{to:o}=j();"
            "x.success(t(`global.save_success`)),i();"
            "o(`/@manage/storages/edit/${e}`)"
        )
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertEqual(count, 0)
        self.assertEqual(patched, text)
        self.assertIn("to:", reason)

    def test_two_router_destructurings_in_one_chunk_abort_the_patch(self):
        """The back binding is read from one place in the file and the call is
        rewritten somewhere else; nothing ties them to the same function. Today
        the storages editor has exactly one of each. A bundle that merged two
        editors into one chunk would have the first destructuring picked
        arbitrarily, and the rewritten call could name a binding that is not in
        scope there -- valid syntax, ReferenceError at runtime."""
        text = (
            "var a=()=>{let {params:p,back:z,to:w}=k();w(`/@manage/storages/edit/${x}`)};"
            + ADD_OR_EDIT
        )
        patched, count, reason = inject.patch_save_navigation(text)
        self.assertEqual(count, 0)
        self.assertEqual(patched, text)
        self.assertIn("destructurings", reason)

    def test_the_dictionary_literal_closes_with_a_comma(self):
        """It is spliced in front of an existing key, so without the trailing
        comma the chunk is a syntax error and the whole bundle fails to parse
        -- while still containing the `StrmSync:{` that a laxer test looks
        for."""
        self.assertEqual(
            inject.dictionary_for({"paths": "源路径"}), 'StrmSync:{"paths":`源路径`},'
        )

    def test_a_translation_that_looks_like_javascript_is_escaped(self):
        """Values land inside a template literal, so a backtick ends the string
        and `${` starts an interpolation."""
        self.assertEqual(inject.js_literal("a`b"), "`a\\`b`")
        self.assertEqual(inject.js_literal("${x}"), "`\\${x}`")
        self.assertEqual(inject.js_literal("a\\b"), "`a\\\\b`")


class InjectionTest(unittest.TestCase):
    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.dist = self.tmp / "dist"
        (self.dist / "assets").mkdir(parents=True)
        (self.dist / "assets" / "entry-AAA.js").write_text(ENTRY_CHUNK, encoding="utf-8")
        (self.dist / "assets" / "AddOrEdit-BBB.js").write_text(ADD_OR_EDIT, encoding="utf-8")
        (self.dist / "assets" / "manage-CCC.js").write_text(
            'import("./entry-AAA.js");import("./AddOrEdit-BBB.js");', encoding="utf-8"
        )
        (self.dist / "index.html").write_text(
            '<script src="/assets/manage-CCC.js"></script>', encoding="utf-8"
        )
        self.i18n = self.tmp / "i18n.json"
        self.i18n.write_text(
            json.dumps({"zh-CN": {"paths": "源路径", "localModes": {"insert": "新增"}}}),
            encoding="utf-8",
        )

    def run_injector(self):
        return inject.main([str(self.dist), str(self.i18n)])

    def entry(self):
        return (self.dist / "assets" / "entry-AAA.js").read_text(encoding="utf-8")

    def assets(self):
        return sorted(p.name for p in (self.dist / "assets").iterdir())
    def addoredit(self):
        return (self.dist / "assets" / "AddOrEdit-BBB.js").read_text(encoding="utf-8")

    def test_both_chunks_are_patched_in_place(self):
        """In place, deliberately. Renaming a patched chunk leaves its unpatched
        importer holding a cached URL that no longer resolves."""
        self.assertEqual(self.run_injector(), 0)
        self.assertEqual(
            self.assets(), ["AddOrEdit-BBB.js", "entry-AAA.js", "manage-CCC.js"]
        )
        importer = (self.dist / "assets" / "manage-CCC.js").read_text(encoding="utf-8")
        self.assertIn('"./entry-AAA.js"', importer)
        self.assertIn('"./AddOrEdit-BBB.js"', importer)

    def test_the_chunks_carry_the_patches(self):
        self.run_injector()
        self.assertIn("StrmSync:{", self.entry())
        self.assertIn("o(`/@manage/storages`)", self.addoredit())

    def test_the_dictionary_lands_beside_the_upstream_driver_not_inside_it(self):
        """Placement is the entire patch. One brace off and the keys land at
        `drivers.Strm.StrmSync.*`, which nothing ever looks up; one comma off
        and the chunk does not parse. Both still contain `StrmSync:{`, which is
        all a laxer assertion checks."""
        self.assertEqual(self.run_injector(), 0)
        text = self.entry()
        self.assertIn('drivers:{Local:{},StrmSync:{"paths":`源路径`', text)
        self.assertIn("},Strm:{PathPrefix:", text)
        self.assertNotIn("Strm:{StrmSync:", text)

    def test_running_the_injector_twice_does_not_stack_a_second_dictionary(self):
        """`StrmSync:{` does not contain `Strm:{`, so an already-patched chunk
        still matches the anchor. Only the already-patched check stops the
        second run from splicing a duplicate in front of it."""
        self.assertEqual(self.run_injector(), 0)
        before = self.entry()
        self.run_injector()
        self.assertEqual(self.entry(), before)
        self.assertEqual(before.count("StrmSync:{"), 1)

    def test_only_the_first_anchor_in_a_chunk_takes_the_dictionary(self):
        """One driver dictionary per chunk. The real chunks carry a second
        `Strm:{` inside a map of per-driver alerts; a StrmSync key spliced in
        there is at best dead weight."""
        (self.dist / "assets" / "entry-AAA.js").write_text(
            ENTRY_CHUNK + ENTRY_CHUNK, encoding="utf-8"
        )
        self.assertEqual(self.run_injector(), 0)
        self.assertEqual(self.entry().count("StrmSync:{"), 1)

    def test_each_language_chunk_gets_the_dictionary_for_its_own_language(self):
        """Three languages ship, and the probes are the only thing deciding
        which chunk is which. A chunk handed the wrong one is not detectable
        anywhere downstream: the keys all resolve, just in the wrong script."""
        self.i18n.write_text(
            json.dumps(
                {
                    "en": {"paths": "Source paths"},
                    "zh-CN": {"paths": "源路径"},
                    "zh-TW": {"paths": "來源路徑"},
                }
            ),
            encoding="utf-8",
        )
        chunk = "var y={drivers:{Strm:{PathPrefix:`%s`,paths:`%s`}}};export{y};"
        (self.dist / "assets" / "lang-EN.js").write_text(
            chunk % ("PathPrefix", "Paths"), encoding="utf-8"
        )
        (self.dist / "assets" / "lang-TW.js").write_text(
            chunk % ("路徑前綴", "路徑"), encoding="utf-8"
        )
        self.assertEqual(self.run_injector(), 0)
        self.assertIn(
            "`Source paths`",
            (self.dist / "assets" / "lang-EN.js").read_text(encoding="utf-8"),
        )
        self.assertIn(
            "`來源路徑`",
            (self.dist / "assets" / "lang-TW.js").read_text(encoding="utf-8"),
        )

    def test_a_dist_with_no_dictionary_fails_the_build(self):
        (self.dist / "assets" / "entry-AAA.js").write_text("var x={};", encoding="utf-8")
        self.assertEqual(self.run_injector(), 1)

    def test_nothing_is_written_when_the_dictionary_anchor_is_missing(self):
        """The navigation patch is applied before the dictionary is even
        looked at. Writing it out before the anchor check would leave a patched
        chunk on disk after a failed run, and the next run would report that no
        navigation patch had been applied -- while one had."""
        (self.dist / "assets" / "entry-AAA.js").write_text("var x={};", encoding="utf-8")
        before = self.addoredit()
        self.assertEqual(self.run_injector(), 1)
        self.assertEqual(self.addoredit(), before)
        self.assertNotIn("o(`/@manage/storages`)", self.addoredit())

    def test_a_bundle_that_lost_the_save_handler_only_warns(self):
        """The navigation fix is a papercut, not a correctness fix, so losing
        the anchor must not stop the image from building -- but exiting 0 is
        only half the contract. A silent success here is a fork that quietly
        stopped applying the patch."""
        (self.dist / "assets" / "AddOrEdit-BBB.js").write_text("var z=()=>{};", encoding="utf-8")
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            self.assertEqual(self.run_injector(), 0)
        self.assertIn("warning:", err.getvalue())
        self.assertIn("no chunk was patched", err.getvalue())

    def test_a_translation_naming_another_editor_cannot_veto_the_navigation_patch(self):
        """The scope check reads every `/@manage/<x>/edit/` in the chunk. If
        the dictionary went in first, a tips string mentioning another editor's
        route would silently disable the navigation patch in a chunk that
        happened to hold both."""
        self.i18n.write_text(
            json.dumps({"zh-CN": {"paths": "见 `/@manage/shares/edit/` 页"}}),
            encoding="utf-8",
        )
        (self.dist / "assets" / "entry-AAA.js").write_text(
            ENTRY_CHUNK + ADD_OR_EDIT, encoding="utf-8"
        )
        (self.dist / "assets" / "AddOrEdit-BBB.js").unlink()
        self.assertEqual(self.run_injector(), 0)
        text = self.entry()
        self.assertIn("StrmSync:{", text)
        self.assertIn("o(`/@manage/storages`)", text)


if __name__ == "__main__":
    unittest.main(verbosity=2)
