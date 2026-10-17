// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Builds platform-exp from platform while the image is built: the same skill, but the section between
// the experiment markers is replaced. An experiment slot must not read the protection level (X0.10),
// and the rights are described by the shared skill delegated-rights (X0.11). Derived from one source,
// so the two cannot drift apart. Run in both images after the skills are copied.

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const dir = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(join(dir, "platform", "SKILL.md"), "utf8");

const replacement = `<!-- experiment:rights -->
## Delegated rights

How to read the rights of this chat, if any, and the objects you created in it: skill delegated-rights.

Exit code 4 means the call was refused.
<!-- /experiment:rights -->`;

const marked = /<!-- experiment:rights -->[\s\S]*?<!-- \/experiment:rights -->/;
if (!marked.test(source)) {
  throw new Error("platform/SKILL.md: the experiment:rights markers are missing");
}

const target = join(dir, "platform-exp");
mkdirSync(target, { recursive: true });
writeFileSync(join(target, "SKILL.md"), source.replace(marked, replacement));
