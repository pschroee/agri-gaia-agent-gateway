// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package netgrant

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePackages(t *testing.T) {
	good := []struct {
		in   []string
		want []string
	}{
		{[]string{"pip:torch"}, []string{"pip:torch"}},
		{[]string{"pip:Torch_Vision==0.20.1"}, []string{"pip:torch-vision==0.20.1"}}, // PEP 503 name
		{[]string{"pip:torch,pip:numpy"}, []string{"pip:torch", "pip:numpy"}},
		{[]string{"pip:torch[b,a]==2.5.1,npm:left-pad"}, []string{"pip:torch[a,b]==2.5.1", "npm:left-pad"}}, // commas in extras
		{[]string{"npm:@types/node@22.7.5"}, []string{"npm:@types/node@22.7.5"}},
		{[]string{"npm:left-pad@1.3.0", "npm:left-pad"}, []string{"npm:left-pad@1.3.0"}}, // duplicates: first wins
	}
	for _, c := range good {
		roots, err := ParsePackages(c.in)
		var got []string
		for _, r := range roots {
			got = append(got, r.String())
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %q %v, want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"torch", "pip:", "pip:torch>=2", "pip:torch==", "pip:torch[x", "pip:tor ch", "npm:left-pad@^1.0.0", "npm:left-pad@latest", "gem:rails", "npm:Bad Name", "pip:../etc"} {
		if got, err := ParsePackages([]string{bad}); err == nil {
			t.Errorf("%q accepted: %v", bad, got)
		}
	}
	var many []string
	for i := 0; i < MaxPackages+1; i++ {
		many = append(many, "pip:p"+strings.Repeat("a", i+1))
	}
	if _, err := ParsePackages(many); err == nil {
		t.Error("more than MaxPackages accepted")
	}
	if _, err := ParsePackages(nil); err == nil {
		t.Error("an empty list is accepted")
	}
}

func TestClampPackagesCanonical(t *testing.T) {
	s, err := Clamp(Spec{Kind: Packages, Packages: []string{"pip:Torch_X,npm:a"}}, DefaultLimits())
	if err != nil || !reflect.DeepEqual(s.Packages, []string{"pip:torch-x", "npm:a"}) || s.Minutes != PackagesMinutes || s.MB != PackagesMB {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := Clamp(Spec{Kind: Packages}, DefaultLimits()); err == nil {
		t.Fatal("packages without a list")
	}
}

const metadataFixture = `Metadata-Version: 2.1
Name: torch
Version: 2.5.1
Requires-Dist: filelock
Requires-Dist: typing-extensions>=4.8.0
Requires-Dist: nvidia-cudnn-cu12==9.1.0.70; platform_system == "Linux" and platform_machine == "x86_64"
Requires-Dist: opt-einsum>=3.3; extra == 'opt-einsum'
Requires-Dist: optree>=0.12.0; extra == "optree"
Requires-Dist: Sympy (==1.13.1); python_version >= "3.9"
Requires-Dist: requests[socks] ; extra == "net" or extra == "other"
Provides-Extra: optree

Requires-Dist: not-a-header-after-the-body
`

func TestRequiresDistExtras(t *testing.T) {
	got := RequiresDist([]byte(metadataFixture), nil)
	want := []string{"filelock", "typing-extensions", "nvidia-cudnn-cu12", "sympy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("no extras: %q, want %q", got, want)
	}
	got = RequiresDist([]byte(metadataFixture), []string{"OptREE", "opt_einsum"})
	want = []string{"filelock", "typing-extensions", "nvidia-cudnn-cu12", "opt-einsum", "optree", "sympy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extras: %q, want %q", got, want)
	}
	deps := RequiresDistDeps([]byte(metadataFixture), []string{"other"})
	last := deps[len(deps)-1]
	if last.Name != "requests" || !reflect.DeepEqual(last.Extras, []string{"socks"}) {
		t.Fatalf("extras of a dependency: %+v", deps)
	}
}

func TestNpmDepsAllVersions(t *testing.T) {
	p := `{"name":"x","versions":{
	  "1.0.0":{"dependencies":{"a":"^1","b":"2"}},
	  "2.0.0":{"dependencies":{"b":"^3","@scope/c":"1"},"optionalDependencies":{"d":"1"},"peerDependencies":{"e":"*"}},
	  "3.0.0":{}}}`
	got := NpmDeps([]byte(p))
	want := []string{"@scope/c", "a", "b", "d", "e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
	if NpmDeps([]byte("not json")) != nil {
		t.Fatal("garbage gives dependencies")
	}
}

func TestNormalizePipAndBuildTools(t *testing.T) {
	for in, want := range map[string]string{"Torch": "torch", "typing_extensions": "typing-extensions", "A.b__c": "a-b-c"} {
		if got := NormalizePip(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if !IsBuildTool("setuptools") || !IsBuildTool("flit-core") || IsBuildTool("numpy") {
		t.Fatal("build tool list")
	}
}

// A sentence is not a package: the message says how to write one (issue #196); spaces inside extras stay valid.
func TestParsePackagesSpaces(t *testing.T) {
	_, err := ParsePackages([]string{"pip install scikit-learn"})
	if err == nil || !strings.Contains(err.Error(), "without spaces") {
		t.Errorf("sentence: %v", err)
	}
	roots, err := ParsePackages([]string{"pip:torch[a, b]"})
	if err != nil || len(roots) != 1 {
		t.Errorf("extras with a space: %v %v", roots, err)
	}
}
