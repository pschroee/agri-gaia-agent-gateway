// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/delegation"
	"agw/internal/platform"
	"agw/internal/store"
)

// edgeJWT builds a token the gateway reads the user's name from: the guard compares the repository
// of an image with the namespace of the user (preferred_username), not with the chat's owner.
func edgeJWT(username string) string {
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]any{"sub": "u-1", "preferred_username": username, "azp": "agw-agent"})
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

// withEdgePlatform attaches a fake platform that serves an image list and answers the build, the
// creation and the deploy of a deployment. It returns the calls that arrived, so that a test can show
// that a refused call never got there.
func withEdgePlatform(t *testing.T, e *env, images, user string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, edgeJWT(user))
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/container-images":
			fmt.Fprint(w, images)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/container-images/"):
			fmt.Fprint(w, `[{"id":5,"repository":"test/x","tag":"v1"}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/container-images/build":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":7}`)
		case r.Method == http.MethodPost && r.URL.Path == "/container-deployments":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":9,"name":"agw-mnist","status":"created"}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/deploy"):
			w.Header().Set("Location", "/tasks/5")
			w.WriteHeader(http.StatusAccepted)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := platform.New(platform.Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: user, Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.opt.Platform = c
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// deploymentCall is a creation of a deployment for image 5 on device 1, with a valid name and port.
func deploymentCall(name string) platform.Request {
	return platform.Request{Method: http.MethodPost, Path: "/container-deployments",
		Body: json.RawMessage(`{"edge_device_id":1,"container_image_id":5,"name":"` + name + `","port_bindings":[{"host_port":"18190","container_port":"8000","protocol":"tcp"}]}`)}
}

// imageList is an image of the given tag as the platform's image list shows it.
func imageList(repository, tag string, ports ...string) string {
	exposed, _ := json.Marshal(ports) // without ports: null
	return fmt.Sprintf(`[{"id":5,"repository":"%s","tag":"%s","platform":"linux/amd64","exposed_ports":%s}]`, repository, tag, exposed)
}

// approveCall starts a call, approves its wait and returns the result.
func approveCall(t *testing.T, e *env, chatID, slot string, req platform.Request) platform.Result {
	t.Helper()
	done := startCall(e, chatID, slot, req)
	ap := pendingApproval(t, e, chatID)
	if _, err := e.m.Decide(context.Background(), ap.ID, true); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// A deployment the chat created itself may be started (edge D1.8): the guard lets it through, and
// starting is compute, so the user is asked for it.
func TestEdgeGuardOwnDeployment(t *testing.T) {
	e := setup(t)
	got := withEdgePlatform(t, e, `[]`, "test")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "9"); err != nil {
		t.Fatal(err)
	}
	done := startCall(e, c.ID, slot, platform.Request{Method: http.MethodPut, Path: "/container-deployments/9/deploy"})
	ap := pendingApproval(t, e, c.ID)
	if ap.Effect != string(delegation.EffectCompute) {
		t.Fatalf("approval of a deploy: effect %q", ap.Effect)
	}
	if _, err := e.m.Decide(ctx, ap.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != "ok" || r.HTTPStatus != http.StatusAccepted {
		t.Fatalf("deploy: %+v", r)
	}
	if calls := got(); !reached(calls, "PUT /container-deployments/9/deploy") {
		t.Fatalf("the deploy did not reach the platform: %v", calls)
	}
}

// reached reports whether one of the calls that arrived is this one.
func reached(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// A deployment of somebody else is out of reach, and that holds without a delegation too (edge D1.8):
// the agent neither starts, nor takes back, nor deletes it, and nobody is asked for it.
func TestEdgeGuardForeignDeploymentRefused(t *testing.T) {
	e := setup(t)
	got := withEdgePlatform(t, e, `[]`, "test")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{}) // no delegation at all
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	want := platform.RefusedPrefix + fmt.Sprintf(msgForeignDeployment, "9")
	for _, req := range []platform.Request{
		{Method: http.MethodPut, Path: "/container-deployments/9/deploy"},
		{Method: http.MethodPut, Path: "/container-deployments/9/undeploy"},
		{Method: http.MethodDelete, Path: "/container-deployments/9"},
	} {
		res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "denied" || res.Message != want {
			t.Fatalf("%s %s: %+v", req.Method, req.Path, res)
		}
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("a foreign deployment asked the user: %+v", aps)
	}
	if calls := got(); len(calls) != 0 {
		t.Fatalf("a foreign deployment reached the platform: %v", calls)
	}
}

// A new deployment needs an image from this chat or from the user's own namespace (edge D1.8): the
// platform would pull any image with its own credentials.
func TestEdgeGuardImageNamespace(t *testing.T) {
	e := setup(t)
	got := withEdgePlatform(t, e, imageList("other/x", "v1", "8000/tcp"), "test")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.PlatformCall(ctx, c.ID, e.m.live[c.ID].slot.ID, "cli", deploymentCall("agw-mnist"))
	if err != nil {
		t.Fatal(err)
	}
	want := platform.RefusedPrefix + fmt.Sprintf(msgForeignImage, "other/x:v1", "test")
	if res.Status != "denied" || res.Message != want {
		t.Fatalf("foreign namespace: %+v", res)
	}
	if aps, _ := e.st.ListApprovals(ctx, store.ApprovalPending, c.ID); len(aps) != 0 {
		t.Fatalf("a foreign image asked the user: %+v", aps)
	}
	// Only reads went out (the image list the guard checked); the creation did not.
	for _, c := range got() {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("the refused creation reached the platform: %v", got())
		}
	}
}

// An image of a foreign namespace is allowed when this chat built it itself (edge D1.8).
func TestEdgeGuardImageOfChat(t *testing.T) {
	e := setup(t)
	withEdgePlatform(t, e, imageList("other/x", "v1", "8000/tcp"), "test")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerImage, "other/x:v1"); err != nil {
		t.Fatal(err)
	}
	res, allowed := e.m.PlatformPrecheck(ctx, c.ID, deploymentCall("agw-mnist"))
	if !allowed {
		t.Fatalf("an image of this chat was refused: %+v", res)
	}
}

// A training image never reaches a device (edge D1.10), also not when the chat built it itself.
func TestEdgeGuardTrainingImage(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	refuse := func(images, wantRef string, chat ChatView) {
		t.Helper()
		withEdgePlatform(t, e, images, "test")
		// Registered in the chat as well: the training image stays refused.
		if err := e.st.AddDelegationObject(ctx, chat.ID, delegation.ContainerImage, wantRef); err != nil {
			t.Fatal(err)
		}
		res, err := e.m.PlatformCall(ctx, chat.ID, e.m.live[chat.ID].slot.ID, "cli", deploymentCall("agw-mnist"))
		if err != nil {
			t.Fatal(err)
		}
		want := platform.RefusedPrefix + fmt.Sprintf(msgTrainingImage, wantRef)
		if res.Status != "denied" || res.Message != want {
			t.Fatalf("%s: %+v", wantRef, res)
		}
	}
	refuse(imageList("test/mnist", "v1-train"), "test/mnist:v1-train", c)
	// Jupyter in the user's own namespace, built in this chat: still a training image.
	refuse(imageList("test/jupyter", "v1", "8888/tcp"), "test/jupyter:v1", c)
	// TensorBoard as well.
	c2, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	refuse(imageList("test/notebook", "v1", "6006/tcp"), "test/notebook:v1", c2)
}

// Images, deployments and the tasks of a deployment are recorded in every chat, also without a
// delegation (edge D1.7): the guard needs them live.
func TestProvenanceWithoutDelegation(t *testing.T) {
	e := setup(t)
	withEdgePlatform(t, e, imageList("test/x", "v1", "8000/tcp"), "test")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{}) // no delegation at all
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID

	build := platform.Request{Method: http.MethodPost, Path: "/container-images/build",
		Body: json.RawMessage(`{"container_template_id":2,"model_id":3,"repository":"test/x","tag":"v1","architecture":"amd64"}`)}
	if r := approveCall(t, e, c.ID, slot, build); r.Status != "ok" {
		t.Fatalf("build: %+v", r)
	}
	if own, err := e.st.IsDelegationObject(ctx, c.ID, delegation.ContainerImage, "test/x:v1"); err != nil || !own {
		t.Fatalf("the built image is not in the register: %v %v", own, err)
	}
	if r := approveCall(t, e, c.ID, slot, deploymentCall("agw-mnist")); r.Status != "ok" {
		t.Fatalf("create: %+v", r)
	}
	if own, err := e.st.IsDelegationObject(ctx, c.ID, delegation.ContainerDeployment, "9"); err != nil || !own {
		t.Fatalf("the created deployment is not in the register: %v %v", own, err)
	}
	if r := approveCall(t, e, c.ID, slot, platform.Request{Method: http.MethodPut, Path: "/container-deployments/9/deploy"}); r.Status != "ok" {
		t.Fatalf("deploy: %+v", r)
	}
	if own, err := e.st.IsDelegationObject(ctx, c.ID, delegation.Task, "5"); err != nil || !own {
		t.Fatalf("the task of the deployment is not in the register: %v %v", own, err)
	}
}

// Reading an image the chat knows records the number the platform gave it (edge D1.9); an image this
// chat does not know stays untouched.
func TestImageAliasOnRead(t *testing.T) {
	e := setup(t)
	withEdgePlatform(t, e, `[]`, "test")
	ctx := context.Background()
	c, err := e.m.Create(ctx, NewChat{})
	if err != nil {
		t.Fatal(err)
	}
	slot := e.m.live[c.ID].slot.ID
	read := func(ref string) {
		t.Helper()
		res, err := e.m.PlatformCall(ctx, c.ID, slot, "cli", platform.Request{Method: http.MethodGet, Path: "/container-images/" + ref})
		if err != nil || res.Status != "ok" {
			t.Fatalf("read %s: %+v %v", ref, res, err)
		}
	}
	// An unknown image is not made known by reading it.
	read("other/y:v2")
	if own, _ := e.st.IsDelegationObject(ctx, c.ID, delegation.ContainerImage, "5"); own {
		t.Fatal("an unknown image got a register entry")
	}
	if err := e.st.AddDelegationObject(ctx, c.ID, delegation.ContainerImage, "test/x:v1"); err != nil {
		t.Fatal(err)
	}
	read("test/x:v1")
	if own, err := e.st.IsDelegationObject(ctx, c.ID, delegation.ContainerImage, "5"); err != nil || !own {
		t.Fatalf("the identifier of the image was not recorded: %v %v", own, err)
	}
}
