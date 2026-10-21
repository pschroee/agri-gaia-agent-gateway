// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package pkggate

import "testing"

func TestParseRoute(t *testing.T) {
	ok := []struct {
		path string
		want Route
	}{
		{"/index/Torch/", Route{Eco: "pip", Kind: KindIndex, Name: "torch"}},
		{"/index/torch", Route{Eco: "pip", Kind: KindIndex, Name: "torch"}},
		{"/index/torch/torch-2.5.1-cp311-cp311-manylinux1_x86_64.whl", Route{Eco: "pip", Kind: KindFile, Name: "torch", File: "torch-2.5.1-cp311-cp311-manylinux1_x86_64.whl", Version: "2.5.1"}},
		{"/index/torch/torch-2.5.1-cp311-cp311-manylinux1_x86_64.whl.metadata", Route{Eco: "pip", Kind: KindMetadata, Name: "torch", File: "torch-2.5.1-cp311-cp311-manylinux1_x86_64.whl.metadata", Version: "2.5.1"}},
		{"/index/typing-extensions/typing_extensions-4.12.2.tar.gz", Route{Eco: "pip", Kind: KindFile, Name: "typing-extensions", File: "typing_extensions-4.12.2.tar.gz", Version: "4.12.2"}},
		{"/npm/left-pad", Route{Eco: "npm", Kind: KindPackument, Name: "left-pad"}},
		{"/npm/@types/node", Route{Eco: "npm", Kind: KindPackument, Name: "@types/node"}},
		{"/npm/@types%2fnode", Route{Eco: "npm", Kind: KindPackument, Name: "@types/node"}},
		{"/npm/left-pad/-/left-pad-1.3.0.tgz", Route{Eco: "npm", Kind: KindTarball, Name: "left-pad", File: "left-pad-1.3.0.tgz", Version: "1.3.0"}},
		{"/npm/@types/node/-/node-22.7.5.tgz", Route{Eco: "npm", Kind: KindTarball, Name: "@types/node", File: "node-22.7.5.tgz", Version: "22.7.5"}},
	}
	for _, c := range ok {
		got, good := ParseRoute(c.path)
		if !good || got != c.want {
			t.Errorf("%s: %+v %v, want %+v", c.path, got, good, c.want)
		}
	}
	for _, p := range []string{"/index/", "/index", "/npm/", "/npm/-/npm/v1/security/advisories/bulk", "/index/torch/evil-1.0-py3-none-any.whl", "/index/torch/../x/", "/index/torch/readme.txt", "/other"} {
		if r, good := ParseRoute(p); good {
			t.Errorf("%s accepted: %+v", p, r)
		}
	}
}
