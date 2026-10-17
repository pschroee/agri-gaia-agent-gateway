// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

// GET /api/exp/environment (X8.5, issue #133): the identity of the gateway build, the checksums of the
// model catalogue, the skills and the rights instruction, and the image IDs of the warm experiment slots.
// The runner compares this answer with plan.lock before it starts a main plan; a run records the image
// IDs of its own slot.

import (
	"net/http"
	"sort"
	"strings"

	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/exp"
	"agw/internal/version"
)

// BuildEnvironment reads the identity of this build once at startup (X8.1/X8.2). The catalogue and the
// skill directory are files of the image; a missing file leaves the checksum empty, which the lock check
// reports instead of silently accepting it.
func BuildEnvironment(env config.Env) exp.Environment {
	catalog, _ := exp.FileSHA256(env.CatalogPath)
	skills, _ := exp.TreeSHA256(env.SkillsPath)
	return exp.Environment{
		GatewayVersion:    version.String(),
		GatewayCommit:     version.Commit,
		GatewayDescribe:   version.Describe,
		CatalogSHA256:     catalog,
		SkillsSHA256:      skills,
		InstructionSHA256: chat.RightsInstructionSHA256(),
	}
}

// environment returns the build identity and the warm experiment slots. Tests may leave Server.Environment
// zero; the identity of the binary and the instruction wording are then filled here.
func (s *Server) environment() exp.Environment {
	out := s.Environment
	if out.GatewayVersion == "" {
		out.GatewayVersion = version.String()
	}
	if out.GatewayCommit == "" {
		out.GatewayCommit = version.Commit
	}
	if out.GatewayDescribe == "" {
		out.GatewayDescribe = version.Describe
	}
	if out.InstructionSHA256 == "" {
		out.InstructionSHA256 = chat.RightsInstructionSHA256()
	}
	out.Slots = nil
	out.ImagesPi, out.ImagesExec = nil, nil
	piImages, execImages := map[string]bool{}, map[string]bool{}
	if s.Pool != nil {
		for _, in := range s.Pool.Snapshot() {
			if !strings.HasPrefix(in.Variant, "exp:") {
				continue
			}
			slot := exp.EnvironmentSlot{ID: in.ID, Variant: in.Variant, State: string(in.State), ChatID: in.ChatID}
			if sl, ok := s.Pool.Get(in.ID); ok && sl.Worker != nil {
				if w, ok := sl.Worker.(interface {
					ImageID() string
					ExecImageID() string
				}); ok {
					slot.ImagePi, slot.ImageExec = w.ImageID(), w.ExecImageID()
				}
			}
			if slot.ImagePi != "" {
				piImages[slot.ImagePi] = true
			}
			if slot.ImageExec != "" {
				execImages[slot.ImageExec] = true
			}
			out.Slots = append(out.Slots, slot)
		}
	}
	out.ImagesPi = sortedKeys(piImages)
	out.ImagesExec = sortedKeys(execImages)
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expEnvironment answers the environment the runner checks a main plan against.
func (s *Server) expEnvironment(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.environment())
}
