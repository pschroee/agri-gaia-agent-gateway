---
name: delegated-rights
description: The user may delegate rights for a chat: which actions on which platform objects. The rights tool shows the rights of this chat, if any, and the objects you created in it.
---

<!-- SPDX-FileCopyrightText: 2026 Philipp Schröer
     SPDX-License-Identifier: MIT -->

# Delegated rights

The user can delegate rights for a chat: which actions on which platform objects. The rights tool
shows the rights of this chat, if any, and the objects you created in it.

| Binding | How to read the rights |
|---|---|
| Command line | `agw-platform rights` |
| MCP | the tool `mcp_platform_rights` |
| REST API | the tool `platform_http` with `GET /_agw/rights` |

The answer is plain text: a heading, one line per rule (`action resource: objects`), and the objects
created in this chat. An empty chat answers that no rights are delegated.
