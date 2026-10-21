# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
"""Tests of agw_run_notebook.py. They need nbformat, nbclient and ipykernel (the exec image of the sandbox);
run them there: python3 -m unittest internal/notebook/launcher/test_agw_run_notebook.py"""
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest

import nbformat
from nbformat.v4 import new_code_cell, new_markdown_cell, new_notebook

LAUNCHER = os.path.join(os.path.dirname(os.path.abspath(__file__)), "agw_run_notebook.py")


def write(d, name, nb):
    p = os.path.join(d, name)
    nbformat.write(nb, p)
    return p


def run(d, entry, wait=True):
    os.makedirs(os.path.join(d, ".agw"), exist_ok=True)
    cmd = [sys.executable, "-u", LAUNCHER, entry, "--out", "executed-" + entry, "--markers", ".agw"]
    p = subprocess.Popen(cmd, cwd=d, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    if not wait:
        return p
    out, _ = p.communicate(timeout=120)
    return p.returncode, out


class LauncherTest(unittest.TestCase):
    def test_failed_cell_is_marked_and_saved(self):
        with tempfile.TemporaryDirectory() as d:
            write(d, "t.ipynb", new_notebook(cells=[new_code_cell("print('one')"), new_code_cell("1/0")]))
            rc, out = run(d, "t.ipynb")
            self.assertEqual(rc, 1)
            self.assertIn("one", out)
            self.assertEqual(open(os.path.join(d, ".agw", "failed_cell")).read(), "2\tZeroDivisionError")
            nb = nbformat.read(os.path.join(d, "executed-t.ipynb"), as_version=4)
            self.assertEqual(nb.cells[0].outputs[0]["text"], "one\n")
            self.assertEqual(nb.cells[1].outputs[0]["output_type"], "error")

    def test_skip_execution_tag_is_void_and_old_outputs_go(self):
        with tempfile.TemporaryDirectory() as d:
            c1 = new_code_cell("print('ran')")
            c1.metadata["tags"] = ["skip-execution"]
            c1.outputs = [nbformat.v4.new_output("stream", name="stdout", text="FAKE\n")]
            write(d, "t.ipynb", new_notebook(cells=[new_markdown_cell("# x"), c1]))
            rc, out = run(d, "t.ipynb")
            self.assertEqual(rc, 0)
            nb = nbformat.read(os.path.join(d, "executed-t.ipynb"), as_version=4)
            self.assertEqual(nb.cells[1].outputs[0]["text"], "ran\n")
            self.assertEqual(nb.cells[1].metadata, {})

    def test_duplicate_key_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "t.ipynb"), "w") as fh:
                fh.write('{"nbformat": 4, "nbformat": 3, "cells": [], "metadata": {}}')
            rc, out = run(d, "t.ipynb")
            self.assertEqual(rc, 98)
            self.assertIn("duplicate key", out)

    def test_sigterm_gives_124_and_ends_the_kernel(self):
        with tempfile.TemporaryDirectory() as d:
            write(d, "t.ipynb", new_notebook(cells=[new_code_cell("import os\nprint(os.getpid(), flush=True)\nimport time\ntime.sleep(300)")]))
            p = run(d, "t.ipynb", wait=False)
            pid = int(self._first_pid(p))
            p.send_signal(signal.SIGTERM)
            p.wait(timeout=30)
            self.assertEqual(p.returncode, 124)
            time.sleep(1)
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)
            self.assertEqual(open(os.path.join(d, ".agw", "failed_cell")).read(), "1\ttime limit")

    @staticmethod
    def _first_pid(p):
        for line in p.stdout:
            line = line.strip()
            if line.isdigit():
                return line
        raise AssertionError("the kernel printed no pid")


if __name__ == "__main__":
    unittest.main()
