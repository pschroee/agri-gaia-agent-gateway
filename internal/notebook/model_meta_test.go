// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package notebook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"agw/internal/store"
)

// A fetched .onnx file reads outputs/model.json of the same run; a valid file is stored with the fetch, an
// invalid one gives a note, other endings read nothing (#181, NT5).
func TestFetchReadsModelMeta(t *testing.T) {
	st := testStore(t)
	chatID := testChat(t, st)
	hub := newFakeHub()
	docker := &fakeDocker{ids: []string{"c1"}}
	n, _ := testService(t, st, docker, hub)
	ctx := context.Background()
	row := store.NotebookRun{ID: "r20260101120000-abcd", ChatID: chatID, HubUser: "agent-test",
		Server: n.Server(chatID), Entry: "train.py", TimeoutMin: 60, StartedAt: time.Now()}
	if err := st.CreateNotebookRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	meta := `{"class_names":["0","1"],"preprocess":{"color":"gray","size":[28,28],"scale":0.00392156862745098}}`
	docker.reply = func(cmd []string, _ []byte) ([]byte, int) {
		if len(cmd) > 2 && cmd[2] == fetchScript {
			return []byte("ONNX-" + cmd[4]), 0
		}
		if len(cmd) > 2 && cmd[2] == headScript {
			return []byte(meta), 0
		}
		return nil, 0
	}
	_, data, note, err := n.fetch(ctx, chatID, row.ID, "outputs/model.onnx")
	if err != nil || note != "" {
		t.Fatalf("fetch: %v %q", err, note)
	}
	f, ok, err := st.NotebookFetchOf(ctx, chatID, shaOf(data))
	if err != nil || !ok || !strings.Contains(string(f.ModelMeta), `"class_names":["0","1"]`) {
		t.Fatalf("the fetch did not keep model.json: %+v %v %v", f, ok, err)
	}
	if _, ok := docker.call("bash", "-c", headScript); !ok {
		t.Fatalf("model.json was not read: %v", docker.callWords())
	}

	// Another ending: nothing is read.
	docker2 := &fakeDocker{ids: []string{"c1"}}
	n2, _ := testService(t, st, docker2, hub)
	docker2.reply = docker.reply
	if _, _, _, err := n2.fetch(ctx, chatID, row.ID, "outputs/metrics.json"); err != nil {
		t.Fatal(err)
	}
	if _, ok := docker2.call("bash", "-c", headScript); ok {
		t.Error("model.json was read for a file that is no .onnx file")
	}

	// An invalid file: the model is still fetched, the agent gets a note and nothing is stored.
	meta = `{"class_names":["0","1"],"preprocess":{"mean":0.5}}`
	_, data, note, err = n.fetch(ctx, chatID, row.ID, "other/model.onnx")
	if err != nil || !strings.Contains(note, "outputs/model.json of this run is not valid") {
		t.Fatalf("invalid file: %v %q", err, note)
	}
	f, ok, _ = st.NotebookFetchOf(ctx, chatID, shaOf(data))
	if !ok || len(f.ModelMeta) != 0 {
		t.Errorf("an invalid model.json was stored: %+v", f)
	}
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
