import copy
import os
import tempfile
import unittest
from unittest import mock

from verifier import context
from verifier.props import licenses, offline

MIT = ("Permission is hereby granted, free of charge, to any person obtaining a copy\n"
       "of this software and associated documentation files (the \"Software\")")
BSD3 = ("Redistribution and use in source and binary forms, with or without\nmodification, "
        "are permitted provided that the following conditions are met:\n"
        "   * Neither the name of Google Inc. nor the names of its\ncontributors may be used")
GPL = "GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007"


def inventory(npm_license="MIT"):
    return licenses.build([{"name": "example.test/a", "version": "v1.0.0", "license": "BSD-3-Clause",
                            "license_file": "LICENSE", "other_files": []}],
                          [{"name": "pkg", "version": "1.0.0", "license": npm_license}])


class ClassifyTest(unittest.TestCase):
    def test_known_texts(self):
        self.assertEqual(licenses.classify(MIT), "MIT")
        self.assertEqual(licenses.classify(BSD3), "BSD-3-Clause")
        self.assertEqual(licenses.classify("Apache License\n  Version 2.0, January 2004"), "Apache-2.0")
        self.assertEqual(licenses.classify("Mozilla Public License, version 2.0"), "MPL-2.0")
        self.assertEqual(licenses.classify(GPL), licenses.UNKNOWN)

    def test_spdx_expressions(self):
        self.assertTrue(licenses.allowed("MIT"))
        self.assertTrue(licenses.allowed("(MIT OR GPL-3.0-only)"))
        self.assertFalse(licenses.allowed("MIT AND GPL-3.0-only"))
        self.assertFalse(licenses.allowed("GPL-3.0-only"))
        self.assertFalse(licenses.allowed("(MIT"))
        self.assertFalse(licenses.allowed(licenses.UNKNOWN))


class InventoryTest(unittest.TestCase):
    def test_go_entry_reads_the_license_file_in_the_cache(self):
        with tempfile.TemporaryDirectory() as d:
            for name, text in (("LICENSE", BSD3), ("PATENTS", "grant"), ("main.go", "")):
                with open(os.path.join(d, name), "w") as f:
                    f.write(text)
            inv = licenses.go_inventory([{"Path": "main.test", "Main": True, "Dir": d},
                                         {"Path": "example.test/a", "Version": "v1.0.0", "Dir": d},
                                         {"Path": "example.test/b", "Version": "v2.0.0"}])
        self.assertEqual([e["name"] for e in inv], ["example.test/a", "example.test/b"])
        self.assertEqual(inv[0]["license"], "BSD-3-Clause")
        self.assertEqual(inv[0]["other_files"], ["PATENTS"])
        self.assertEqual(inv[1]["license"], licenses.UNKNOWN)

    def test_npm_inventory_from_the_lockfile(self):
        lock = {"packages": {"": {"name": "root", "license": "MIT"},
                             "node_modules/@s/b": {"version": "2.0.0", "license": "ISC"},
                             "node_modules/a": {"version": "1.0.0", "license": "MIT"},
                             "node_modules/x/node_modules/a": {"version": "1.0.0", "license": "MIT"},
                             "node_modules/c": {"version": "3.0.0"}}}
        self.assertEqual(licenses.npm_inventory(lock), [
            {"name": "@s/b", "version": "2.0.0", "license": "ISC"},
            {"name": "a", "version": "1.0.0", "license": "MIT"},
            {"name": "c", "version": "3.0.0", "license": licenses.UNKNOWN}])

    def test_open_and_current_inventory_passes(self):
        ok, details, failures = licenses.check(inventory(), copy.deepcopy(inventory()))
        self.assertTrue(ok, failures)
        self.assertEqual(details["dependencies"], {"go": 1, "npm": 1})

    def test_disallowed_license_fails(self):
        inv = inventory("GPL-3.0-only")
        ok, _, failures = licenses.check(inv, copy.deepcopy(inv))
        self.assertFalse(ok)
        self.assertIn("GPL-3.0-only", failures[0])

    def test_stale_inventory_fails(self):
        versioned = inventory()
        regenerated = inventory()
        regenerated["dependencies"][1]["version"] = "1.0.1"
        ok, details, failures = licenses.check(regenerated, versioned)
        self.assertFalse(ok)
        self.assertEqual(details["stale"]["added"], ["npm pkg 1.0.1"])
        self.assertEqual(details["stale"]["removed"], ["npm pkg 1.0.0"])
        self.assertFalse(licenses.check(inventory(), None)[0])

    def test_inventory_is_one_tagged_list(self):
        inv = inventory()
        self.assertEqual(sorted(inv), ["dependencies", "format"])
        self.assertEqual([(e["ecosystem"], e["name"]) for e in inv["dependencies"]],
                         [("go", "example.test/a"), ("npm", "pkg")])

    def test_versioned_inventory_is_open(self):
        root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
        import json
        with open(os.path.join(root, licenses.INVENTORY)) as f:
            inv = json.load(f)
        ok, _, failures = licenses.check(inv, copy.deepcopy(inv))
        self.assertTrue(ok, failures)


class OfflineTest(unittest.TestCase):
    def test_only_lo_passes(self):
        ok, details, failures = offline.check(["lo"], ["lo"])
        self.assertTrue(ok, failures)
        self.assertEqual(details["interfaces"], ["lo"])

    def test_extra_interface_fails(self):
        self.assertFalse(offline.check(["eth0", "lo"], ["lo"])[0])
        self.assertFalse(offline.check(["lo"], ["eth0", "lo"])[0])

    def test_reads_class_net(self):
        with tempfile.TemporaryDirectory() as d:
            for name in ("lo", "veth0"):
                os.makedirs(os.path.join(d, "class", "net", name))
            self.assertEqual(offline.interfaces(d), ["lo", "veth0"])
            self.assertFalse(offline.check(offline.interfaces(d), ["lo"])[0])

    def test_run_leaves_its_trace(self):
        with tempfile.TemporaryDirectory() as root:
            ctx = context.Context(root, "out", params={})
            with mock.patch.object(offline, "namespace_sysfs_interfaces", return_value=["lo"]), \
                    mock.patch.object(offline.socket, "if_nameindex", return_value=[(1, "lo")]):
                res = offline.run(ctx)
            self.assertEqual(res["trace"], os.path.join("out", "runs", "offline", "interfaces.json"))
            self.assertTrue(os.path.isfile(os.path.join(root, res["trace"])))


if __name__ == "__main__":
    unittest.main()
