#!/usr/bin/env python3
"""Tests for fork_inject_i18n.py. Run: python3 scripts/fork_inject_i18n_test.py

The fixtures are lifted verbatim from a shipped OpenList-Frontend bundle rather
than written by hand, because both patches this script applies are anchored on
minifier output and a hand-written approximation would happily pass while the
real bundle did not match.
"""

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
        patched, count, _ = inject.patch_save_navigation(ENTRY_CHUNK)
        self.assertEqual(count, 0)
        self.assertEqual(patched, ENTRY_CHUNK)


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

    def assets(self):
        return sorted(p.name for p in (self.dist / "assets").iterdir())

    def test_both_patched_chunks_are_renamed_and_repointed(self):
        self.assertEqual(self.run_injector(), 0)
        names = self.assets()
        # Vite hashes these names over upstream's build, so a patch applied
        # afterwards would otherwise ship under a URL browsers have cached for
        # 180 days.
        self.assertNotIn("entry-AAA.js", names)
        self.assertNotIn("AddOrEdit-BBB.js", names)
        entry = next(n for n in names if n.startswith("entry-AAA."))
        addoredit = next(n for n in names if n.startswith("AddOrEdit-BBB."))

        importer = (self.dist / "assets" / "manage-CCC.js").read_text(encoding="utf-8")
        self.assertIn(entry, importer)
        self.assertIn(addoredit, importer)
        self.assertNotIn('"./entry-AAA.js"', importer)
        self.assertNotIn('"./AddOrEdit-BBB.js"', importer)

    def test_the_renamed_chunks_carry_the_patches(self):
        self.run_injector()
        entry = next(p for p in (self.dist / "assets").iterdir() if p.name.startswith("entry-AAA."))
        self.assertIn("StrmSync:{", entry.read_text(encoding="utf-8"))
        addoredit = next(
            p for p in (self.dist / "assets").iterdir() if p.name.startswith("AddOrEdit-BBB.")
        )
        self.assertIn("o(`/@manage/storages`)", addoredit.read_text(encoding="utf-8"))

    def test_a_dist_with_no_dictionary_fails_the_build(self):
        (self.dist / "assets" / "entry-AAA.js").write_text("var x={};", encoding="utf-8")
        self.assertEqual(self.run_injector(), 1)

    def test_a_renamed_chunk_with_no_importer_fails_the_build(self):
        (self.dist / "assets" / "manage-CCC.js").unlink()
        with self.assertRaises(SystemExit):
            self.run_injector()

    def test_a_bundle_that_lost_the_save_handler_only_warns(self):
        """The navigation fix is a papercut, not a correctness fix. Losing the
        anchor should not stop the image from building."""
        (self.dist / "assets" / "AddOrEdit-BBB.js").write_text("var z=()=>{};", encoding="utf-8")
        self.assertEqual(self.run_injector(), 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
