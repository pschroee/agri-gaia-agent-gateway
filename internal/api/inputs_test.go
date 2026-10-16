// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agw/internal/artifacts"
	"agw/internal/chat"
	"agw/internal/config"
	"agw/internal/pool"
	"agw/internal/store"
)

// testBlobs is the object store of the route test: only what AddInput and RemoveInput touch.
type testBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (b *testBlobs) Put(_ context.Context, k string, r io.Reader, _ int64, _ string) error {
	d, _ := io.ReadAll(r)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[k] = d
	return nil
}

func (b *testBlobs) Get(_ context.Context, k string) (io.ReadCloser, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.m[k]
	if !ok {
		return nil, 0, store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(d)), int64(len(d)), nil
}

func (b *testBlobs) Move(_ context.Context, src, dst string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[dst] = b.m[src]
	delete(b.m, src)
	return nil
}

func (b *testBlobs) Delete(_ context.Context, k string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, k)
	return nil
}

func (b *testBlobs) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []string{}
	for k := range b.m {
		out = append(out, k)
	}
	return out
}

// L4/#121: the route over the API — 204 for an unsent input, 404 for an unknown name, 409 already_sent for a file
// that went with a message, 409 chat_in_trash in the trash, 404 for a foreign chat.
func TestRemoveInputRoute(t *testing.T) {
	url := os.Getenv("AGW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.OpenSchema(ctx, url, "apitest_inputs_"+strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DropSchema(context.Background()); st.Close() })
	cat, err := config.ParseCatalog([]byte(`{"default":"p/m","providers":[{"id":"p","upstream":"http://x","api":"openai-completions","models":[{"id":"m"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New[chat.Agent](func(context.Context, string, string) (chat.Agent, error) {
		return nil, errors.New("no slot in the test")
	}, func(context.Context, chat.Agent) {}, map[string]int{"cli": 0})
	blobs := &testBlobs{m: map[string][]byte{}}
	m := chat.NewManager(st, p, cat, blobs, artifacts.NewBroker(), chat.Options{AcquireTimeout: 100 * time.Millisecond,
		MaxSubagents: 5, ArtifactMaxBytes: 1 << 20})
	srv, is := oidcServer(t, m, p, cat, nil)
	a := loggedIn(t, srv, is, anna)
	b := loggedIn(t, srv, is, bert)

	c, err := st.CreateChat(ctx, store.NewChat{Title: "uploads", Model: "p/m", Variant: "cli", Owner: anna.Sub})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetState(ctx, c.ID, store.StateDormant)
	base := srv.URL + "/api/chats/" + c.ID + "/inputs/"

	// path: /api/chats/<id>/inputs/<name>
	if _, err := m.AddInput(ctx, c.ID, "data.csv", []byte("x,y\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddInput(ctx, c.ID, "sent.csv", []byte("s\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkInputsSent(ctx, c.ID, []string{"sent.csv"}); err != nil {
		t.Fatal(err)
	}

	if code, body := call(t, a, "DELETE", base+"gone.csv", ""); code != 404 {
		t.Errorf("unknown input: %d %s", code, body)
	}
	if code, body := call(t, a, "DELETE", base+"sent.csv", ""); code != 409 || !strings.Contains(body, "already_sent") {
		t.Errorf("sent input: %d %s", code, body)
	}
	if code, body := call(t, b, "DELETE", base+"data.csv", ""); code != 404 {
		t.Errorf("foreign chat: %d %s", code, body)
	}
	if code, body := call(t, a, "DELETE", base+"data.csv", ""); code != 204 {
		t.Errorf("unsent input: %d %s", code, body)
	}
	if _, err := st.GetArtifact(ctx, c.ID, store.KindInput, "data.csv"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row still there: %v", err)
	}
	if n := len(blobs.keys()); n != 1 {
		t.Errorf("object store: %d keys (the sent file stays)", n)
	}

	// a chat in the trash answers 409 chat_in_trash (also for the input route)
	if _, err := m.AddInput(ctx, c.ID, "again.csv", []byte("z\n")); err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, a, "POST", srv.URL+"/api/chats/"+c.ID+"/trash", ""); code != 200 {
		t.Fatalf("trash: %d %s", code, body)
	}
	if code, body := call(t, a, "DELETE", base+"again.csv", ""); code != 409 || !strings.Contains(body, "chat_in_trash") {
		t.Errorf("in the trash: %d %s", code, body)
	}
}
