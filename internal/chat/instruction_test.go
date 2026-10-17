// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"strings"
	"testing"

	"agw/internal/store"
)

// X2: the protection level "instruction" gives the agent the delegated rights as an instruction with
// the first message. These tests run against the real store; they skip without AGW_TEST_DATABASE_URL
// like the other chat tests.

const instructionTask = "List my datasets."

// chatAgent returns the fake agent of the chat's slot. An experiment chat runs in a slot of its own
// ("exp:<binding>", X0.3); the pool's warm slot for the configured binding is created first, so
// e.agent(0) is not necessarily the chat's agent. Reading the live slot avoids the race.
func chatAgent(t *testing.T, e *env, chatID string) *fakeAgent {
	t.Helper()
	e.m.mu.Lock()
	l := e.m.live[chatID]
	e.m.mu.Unlock()
	if l == nil || l.slot == nil {
		t.Fatalf("chat %s has no live slot", chatID)
	}
	a, ok := l.slot.Worker.(*fakeAgent)
	if !ok {
		t.Fatalf("chat %s runs in %T, not in a fake agent", chatID, l.slot.Worker)
	}
	return a
}

// sendFirst sends the task and waits until it arrived in the sandbox and the chat is quiet again;
// want is the expected number of messages the chat's sandbox has received.
func sendFirst(t *testing.T, e *env, chatID string, want int) string {
	t.Helper()
	if _, err := e.m.Send(context.Background(), chatID, instructionTask); err != nil {
		t.Fatal(err)
	}
	a := chatAgent(t, e, chatID)
	waitUntil(t, "message", func() bool { return len(a.prompts()) == want })
	waitSettled(t, e, chatID)
	return a.prompts()[want-1]
}

// The instruction goes out with the first message only; the second message stays plain user text.
func TestInstructionOnlyFirstTurn(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	c := createExperiment(t, e, "instruction", NewChat{})

	first := sendFirst(t, e, c.ID, 1)
	if !strings.Contains(first, rightsInstruction) || !strings.Contains(first, "Delegated rights:") {
		t.Fatalf("first message without instruction: %q", first)
	}
	if second := sendFirst(t, e, c.ID, 2); second != instructionTask {
		t.Fatalf("second message: %q", second)
	}
}

// X2.1: the instruction shows up as a source of the first turn (chat_turns.sources), as a context
// note for the agent alone.
func TestInstructionRecordedAsSource(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	ctx := context.Background()
	c := createExperiment(t, e, "instruction", NewChat{})
	sendFirst(t, e, c.ID, 1)
	sendFirst(t, e, c.ID, 2)

	turns, err := e.st.Turns(ctx, c.ID)
	if err != nil || len(turns) != 2 {
		t.Fatalf("turns: %+v %v", turns, err)
	}
	if !hasSource(turns[0].Sources, store.NoteRightsInstruction) {
		t.Errorf("first turn without rights_instruction: %+v", turns[0].Sources)
	}
	if hasSource(turns[1].Sources, store.NoteRightsInstruction) {
		t.Errorf("second turn carries the instruction: %+v", turns[1].Sources)
	}
	for _, s := range turns[0].Sources {
		if s.Type == store.NoteRightsInstruction && s.Audience != store.AudienceAgent {
			t.Errorf("audience %q", s.Audience)
		}
	}
}

// X2.3: the text is word for word the same in "instruction" and "service"; the enforcement does not
// appear.
func TestInstructionSameForInstructionAndService(t *testing.T) {
	var texts []string
	for _, protection := range []string{"instruction", "service"} {
		t.Run(protection, func(t *testing.T) {
			e := setup(t)
			allowExperiment(e)
			c := createExperiment(t, e, protection, NewChat{})
			texts = append(texts, sendFirst(t, e, c.ID, 1))
		})
	}
	if len(texts) != 2 {
		t.Fatalf("texts: %d", len(texts))
	}
	if texts[0] != texts[1] {
		t.Fatalf("instruction and service differ:\n%s\n---\n%s", texts[0], texts[1])
	}
	for _, w := range []string{"violations are", "only logged", "blocked"} {
		if strings.Contains(texts[0], w) {
			t.Errorf("instruction names the enforcement (%q): %s", w, texts[0])
		}
	}
}

// X2: in the level "none" the first message stays plain user text.
func TestNoInstructionInProtectionNone(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	ctx := context.Background()
	c := createExperiment(t, e, "none", NewChat{})
	if first := sendFirst(t, e, c.ID, 1); first != instructionTask {
		t.Fatalf("first message: %q", first)
	}
	turns, err := e.st.Turns(ctx, c.ID)
	if err != nil || len(turns) == 0 {
		t.Fatalf("turns: %+v %v", turns, err)
	}
	if hasSource(turns[0].Sources, store.NoteRightsInstruction) {
		t.Errorf("level none carries the instruction: %+v", turns[0].Sources)
	}
}

// X2.2: the instruction names the delegated rights themselves, without the kind of enforcement.
func TestInstructionListsRules(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	c := createExperiment(t, e, "instruction", NewChat{})
	first := sendFirst(t, e, c.ID, 1)
	for _, line := range []string{"read dataset: all", "delete dataset: all", "run training: all"} {
		if !strings.Contains(first, line) {
			t.Errorf("rule %q missing in:\n%s", line, first)
		}
	}
}

// X2.5: experiment chats are created without automatic compaction, whatever the request says.
func TestExperimentChatNoAutoCompact(t *testing.T) {
	e := setup(t)
	allowExperiment(e)
	ctx := context.Background()
	on := true
	c := createExperiment(t, e, "instruction", NewChat{AutoCompact: &on})
	got, err := e.st.GetChat(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoCompact {
		t.Fatal("experiment chat compacts automatically")
	}
	plain, err := e.m.Create(ctx, NewChat{})
	if err != nil || !plain.AutoCompact {
		t.Fatalf("product chat lost the default: %+v %v", plain, err)
	}
}

func hasSource(src []store.Source, noteType string) bool {
	for _, s := range src {
		if s.Kind == store.QueueSystem && s.Type == noteType {
			return true
		}
	}
	return false
}
