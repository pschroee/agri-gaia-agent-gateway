// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import _ "embed"

// launcherPy is the starter of a notebook run (issue #193). The same file is copied into the run
// directory (.agw/agw_run_notebook.py) and into the sandbox image (/usr/local/bin/agw-run-notebook), so a
// notebook that runs in the sandbox test runs the same way in the notebook server.
//
//go:embed launcher/agw_run_notebook.py
var launcherPy []byte
