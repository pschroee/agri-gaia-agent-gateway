// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package platform

// RefusedPrefix starts the message of a result that the gateway refused before anything reached the
// platform (fail closed, compute limit). The socket logs such a result as "refused" in the activity
// and passes the message on unchanged.
const RefusedPrefix = "refused: "
