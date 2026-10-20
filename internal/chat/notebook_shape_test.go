// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import "agw/internal/platform"

// Shape completes fakeNotebook as a Notebooker (#165).
func (f *fakeNotebook) Shape() platform.NotebookShape { return platform.DefaultNotebookShape }
