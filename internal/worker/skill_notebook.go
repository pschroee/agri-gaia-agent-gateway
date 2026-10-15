// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

// The skill of the notebook tools (own code C-E3): the agent's own code in the chat's notebook server.
// It is loaded in the cli binding like the other command skills; both paths (with and without the
// capability notebook) stand in its description, so the agent knows what to do when the server is not
// there.
const notebookSkill = "/opt/agw/skills/notebook"

func init() { registerCLISkill(notebookSkill) }
