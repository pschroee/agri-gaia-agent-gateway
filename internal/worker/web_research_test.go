// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package worker

import (
	"strings"
	"testing"
)

// web-tools.ts loads the suite from the path the image installs it to and sets the texts the system note
// and the skill rely on. A pi run is not part of the everyday tests; this guards the file.
func TestWebToolsExtension(t *testing.T) {
	s := repoFile(t, "images/agw-basis/ext/web-tools.ts")
	if !strings.Contains(s, `import suite from "`+searxExt+`"`) {
		t.Error("web-tools.ts does not import the suite from " + searxExt)
	}
	for _, w := range []string{
		`tool.name === "web_search"`, `tool.name === "web_extract"`, "Only available while internet is on", "short keyword queries",
		"several short searches", "100000", "Never put passwords, tokens or API keys into a", "use curl instead",
		"with their URL", "if sources disagree", "...(tool.promptGuidelines ?? [])", "SPDX-License-Identifier: MIT",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("web-tools.ts without %q", w)
		}
	}
	if !strings.Contains(repoFile(t, "images/agw-basis/Dockerfile"), "COPY images/agw-basis/ext /opt/agw/ext") {
		t.Error("the pi image does not copy the extensions")
	}
}
