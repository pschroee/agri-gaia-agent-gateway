# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
"""Run a Jupyter notebook cell by cell and keep the executed notebook (issue #193).

usage: agw-run-notebook <entry.ipynb> [--out FILE] [--markers DIR]

The executed notebook is written before the first cell and after every cell, also when a cell fails or the
run is stopped, so the notebook with all outputs so far is always there. The outputs are printed as they come
(the run log). Only the source of the code cells is run: metadata, tags, stored outputs and the kernel name of
the file are replaced, so that what runs is what the approval showed.

Exit codes: 0 done, 1 a cell failed, 124 stopped by the time limit (SIGTERM), 98 notebook refused.
"""
import argparse
import json
import os
import re
import signal
import sys

REFUSED = 98
ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")


def refuse(msg):
    print("agw-run-notebook: " + msg, file=sys.stderr, flush=True)
    sys.exit(REFUSED)


def no_dupes(pairs):
    d = {}
    for k, v in pairs:
        if k in d:
            refuse("duplicate key %r in the notebook" % k)
        d[k] = v
    return d


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("entry")
    ap.add_argument("--out", default=None)
    ap.add_argument("--markers", default=None)
    a = ap.parse_args()
    out = a.out or "executed-" + os.path.basename(a.entry)

    with open(a.entry, "r", encoding="utf-8") as fh:
        text = fh.read()
    try:
        raw = json.loads(text, object_pairs_hook=no_dupes)
    except ValueError as e:
        refuse("%s is not a notebook: %s" % (a.entry, e))
    if not isinstance(raw, dict) or raw.get("nbformat") != 4 or not isinstance(raw.get("cells"), list):
        refuse("%s is not a notebook in nbformat 4" % a.entry)
    for c in raw["cells"]:
        if not isinstance(c, dict):
            refuse("a cell is not an object")
        if c.get("cell_type") == "code":
            s = c.get("source", "")
            if not (isinstance(s, str) or (isinstance(s, list) and all(isinstance(x, str) for x in s))):
                refuse("the source of a code cell is neither text nor a list of text")

    os.environ["MPLBACKEND"] = "module://matplotlib_inline.backend_inline"
    import nbformat
    from nbclient import NotebookClient
    from nbclient.exceptions import CellExecutionError, DeadKernelError

    nb = nbformat.reads(text, as_version=4)
    for c in nb.cells:
        c.metadata = {}
        if c.cell_type == "code":
            c.outputs = []
            c.execution_count = None
    nb.metadata["kernelspec"] = {"name": "python3", "display_name": "Python 3", "language": "python"}

    # The number of a cell is the one the approval shows ("# cell n"): code cells without text are not counted.
    number = {}
    n = 0
    for i, c in enumerate(nb.cells):
        if c.cell_type == "code" and c.source.strip() != "":
            n += 1
            number[i] = n
    state = {"cell": None}

    def save():
        tmp = out + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            nbformat.write(nb, fh)
        os.replace(tmp, out)

    def mark(cell, why):
        if not a.markers or cell is None:
            return
        try:
            with open(os.path.join(a.markers, "failed_cell"), "w", encoding="utf-8") as fh:
                fh.write("%d\t%s" % (cell, why.replace("\n", " ").replace("\t", " ")[:120]))
        except OSError:
            pass

    def say(s):
        print(s, flush=True)

    class Client(NotebookClient):
        def output(self, outs, msg, display_id, cell_index):
            res = super().output(outs, msg, display_id, cell_index)
            t = msg["header"]["msg_type"]
            c = msg["content"]
            try:
                if t == "stream":
                    sys.stdout.write(c.get("text", ""))
                    sys.stdout.flush()
                elif t == "error":
                    say(ANSI.sub("", "\n".join(c.get("traceback", []))))
                elif t in ("display_data", "execute_result"):
                    data = c.get("data", {})
                    if "image/png" in data:
                        say("[image/png]")
                    elif "text/plain" in data:
                        say(data["text/plain"])
            except Exception:  # printing must never stop a run
                pass
            return res

    def on_start(cell, cell_index):
        state["cell"] = number.get(cell_index)
        if state["cell"] is not None:
            say("── cell %d ──" % state["cell"])

    def on_done(cell, cell_index, execute_reply):
        save()

    client = Client(nb, timeout=None, kernel_name="python3", resources={"metadata": {"path": os.getcwd()}},
                    on_cell_start=on_start, on_cell_executed=on_done)

    def on_term(signum, frame):
        try:
            save()
        except Exception:
            pass
        mark(state["cell"], "time limit")
        try:
            os.killpg(client.km.provisioner.process.pid, signal.SIGKILL)
        except Exception:
            pass
        os._exit(124)

    signal.signal(signal.SIGTERM, on_term)
    save()
    code = 0
    try:
        client.execute()
    except CellExecutionError as e:
        mark(state["cell"], getattr(e, "ename", "") or "error")
        code = 1
    except DeadKernelError:
        mark(state["cell"], "DeadKernelError (the kernel died, often out of memory)")
        say("the kernel died (often out of memory)")
        code = 1
    finally:
        save()
    sys.exit(code)


if __name__ == "__main__":
    main()
