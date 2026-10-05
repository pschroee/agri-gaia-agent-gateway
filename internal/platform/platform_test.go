package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakePlatform ist Keycloak und Backend in einem: /token gibt Token aus, alles
// andere verlangt das zuletzt ausgegebene Token.
type fakePlatform struct {
	mu      sync.Mutex
	n       int    // ausgegebene Token
	valid   string // gültiges Token
	grants  []string
	reqs    []string
	bodies  []string
	respond func(w http.ResponseWriter, r *http.Request)
}

func (f *fakePlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		f.grants = append(f.grants, r.Form.Get("grant_type")+":"+r.Form.Get("client_id"))
		if r.Form.Get("grant_type") == "password" && r.Form.Get("password") != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		f.n++
		f.valid = fmt.Sprintf("tok%d", f.n)
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"r%d","expires_in":3600}`, f.valid, f.n)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.valid {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	f.bodies = append(f.bodies, string(b))
	if f.respond != nil {
		f.respond(w, r)
		return
	}
	fmt.Fprint(w, `[ {"id": 1, "name": "mnist"} ]`)
}

func newClient(t *testing.T, f *fakePlatform, max int) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL + "/api", TokenURL: srv.URL + "/token", User: "test", Password: "pw", MaxResult: max})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestNewDisabledWithoutURL(t *testing.T) {
	c, err := New(Config{})
	if c != nil || err != nil {
		t.Fatalf("ohne URL erwartet nil, nil: %v %v", c, err)
	}
	if _, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/datasets"}); err != ErrNotConfigured {
		t.Fatalf("erwartet ErrNotConfigured: %v", err)
	}
	if _, err := New(Config{APIURL: "https://x"}); err == nil {
		t.Fatal("ohne Zugangsdaten erwartet Fehler")
	}
}

func TestDoLoginCompactAndReuse(t *testing.T) {
	f := &fakePlatform{}
	c, _ := newClient(t, f, 0)
	for range 2 {
		res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/datasets", Query: map[string]string{"limit": "5"}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "ok" || res.HTTPStatus != 200 || res.Body != `[{"id":1,"name":"mnist"}]` {
			t.Fatalf("Antwort: %+v", res)
		}
	}
	if len(f.grants) != 1 || f.grants[0] != "password:frontend" {
		t.Fatalf("erwartet genau eine Anmeldung über frontend: %v", f.grants)
	}
	if f.reqs[0] != "GET /api/datasets?limit=5" {
		t.Fatalf("Pfad: %v", f.reqs)
	}
}

func TestDoReloginOn401(t *testing.T) {
	f := &fakePlatform{}
	c, _ := newClient(t, f, 0)
	if _, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/models"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.valid = "widerrufen"
	f.mu.Unlock()
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/models"})
	if err != nil || res.HTTPStatus != 200 {
		t.Fatalf("nach 401 erneut anmelden: %+v %v", res, err)
	}
	if len(f.grants) != 2 {
		t.Fatalf("Anmeldungen: %v", f.grants)
	}
}

func TestDoWrongPassword(t *testing.T) {
	f := &fakePlatform{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, _ := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "test", Password: "falsch"})
	_, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/datasets"})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("erwartet Anmeldefehler: %v", err)
	}
}

func TestDoTruncatesAndLocation(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://"+r.Host+"/api/tasks/7")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, strings.Repeat("x", 100))
	}}
	c, _ := newClient(t, f, 10)
	res, err := c.Do(context.Background(), "", Request{Method: "POST", Path: "/train/containers/1/run"})
	if err != nil {
		t.Fatal(err)
	}
	if res.HTTPStatus != 202 || !res.Truncated || len(res.Body) != 10 || res.Location != "/tasks/7" {
		t.Fatalf("Antwort: %+v", res)
	}
	if !strings.Contains(res.Text(), "gekürzt") || !strings.Contains(res.Text(), "Location: /tasks/7") {
		t.Fatalf("Text: %s", res.Text())
	}
}

func TestDoPlatformErrorIsResult(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"Not Found"}`)
	}}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/datasets/99"})
	if err != nil || res.Status != "error" || res.HTTPStatus != 404 || !strings.Contains(res.Text(), "Not Found") {
		t.Fatalf("Antwort: %+v %v", res, err)
	}
}

func TestNormalize(t *testing.T) {
	bad := []Request{
		{Method: "GET", Path: "datasets"},
		{Method: "GET", Path: "/datasets/../users/me"},
		{Method: "GET", Path: "//evil.example/x"},
		{Method: "GET", Path: "https://evil.example/x"},
		{Method: "GET", Path: "/datasets?limit=1"},
		{Method: "GET", Path: "/datasets/%2e%2e/x"},
		{Method: "GET", Path: "/urls/basic-auth"},
		{Method: "GET", Path: "/URLs/basic-auth/fuseki"},
		{Method: "POST", Path: "/service/user-registration"},
		{Method: "GET", Path: "/users/me"},
		{Method: "TRACE", Path: "/datasets"},
		{Method: "GET", Path: "/datasets", Body: json.RawMessage(`{}`)},
		{Method: "POST", Path: "/train/config", Body: json.RawMessage(`{kaputt`)},
	}
	for _, r := range bad {
		if _, err := Normalize(r); err == nil {
			t.Errorf("erwartet Ablehnung: %+v", r)
		}
	}
	r, err := Normalize(Request{Method: "post", Path: "/train/config", Body: json.RawMessage("{ \"a\" : 1 }")})
	if err != nil || r.Method != "POST" || string(r.Body) != `{"a":1}` || !r.Writes() {
		t.Fatalf("Normalisierung: %+v %v", r, err)
	}
	if r, err := Normalize(Request{Path: "/urlsx"}); err != nil || r.Method != "GET" || r.Writes() {
		t.Fatalf("leere Methode heißt GET, /urlsx ist nicht gesperrt: %+v %v", r, err)
	}
}

func TestToolsBuild(t *testing.T) {
	cases := []struct {
		tool, args, want string
	}{
		{"list_datasets", `{}`, "GET /datasets"},
		{"list_datasets", `{"limit":5,"skip":10}`, "GET /datasets?limit=5&skip=10"},
		{"get_dataset", `{"dataset_id":3}`, "GET /datasets/3"},
		{"datasets", ``, "GET /datasets"},
		{"train_options", `{}`, "GET /train/providers"},
		{"train_options", `{"provider":"Torchvision"}`, "GET /train/architectures/Torchvision"},
		{"train_options", `{"provider":"Torchvision","architecture":"Mask R-CNN"}`, "GET /train/config/Torchvision/Mask R-CNN"},
		{"training_logs", `{"train_container_id":"4","tail":20}`, "GET /train/containers/4/logs?tail=20"},
		{"start_training", `{"train_container_id":4}`, "POST /train/containers/4/run"},
		{"request", `{"method":"patch","path":"/datasets/3","body":{"name":"x"}}`, "PATCH /datasets/3"},
		{"request", `{"method":"GET","path":"/agrovoc/keywords","query":{"keyword":"pig","limit":3}}`, "GET /agrovoc/keywords?keyword=pig&limit=3"},
	}
	for _, c := range cases {
		tool, ok := Lookup(c.tool)
		if !ok {
			t.Fatalf("Werkzeug %s fehlt", c.tool)
		}
		r, err := tool.Build(json.RawMessage(c.args))
		if err != nil || r.String() != c.want {
			t.Errorf("%s %s: %q %v, erwartet %q", c.tool, c.args, r.String(), err, c.want)
		}
	}
	bad := []struct{ tool, args string }{
		{"get_dataset", `{}`},
		{"get_dataset", `{"dataset_id":"3/../../users"}`},
		{"get_dataset", `{"dataset_id":1.5}`},
		{"list_datasets", `{"foo":1}`},
		{"train_options", `{"provider":"../users"}`},
		{"train_options", `{"architecture":"EfficientNet"}`},
		{"create_training", `{"provider":"T","architecture":"E","category":"C","dataset_id":1}`},
		{"create_training", `{"provider":"T","architecture":"E","category":"C","dataset_id":1,"train_config":[1]}`},
		{"request", `{"method":"GET","path":"/urls/basic-auth"}`},
		{"request", `{"method":"GET","path":"/x","query":{"a":{"b":1}}}`},
	}
	for _, c := range bad {
		tool, _ := Lookup(c.tool)
		if r, err := tool.Build(json.RawMessage(c.args)); err == nil {
			t.Errorf("%s %s: erwartet Fehler, gebaut %s", c.tool, c.args, r)
		}
	}
}

func TestCreateTrainingBody(t *testing.T) {
	tool, _ := Lookup("platform_create_training")
	r, err := tool.Build(json.RawMessage(`{"provider":"Torchvision","architecture":"EfficientNet","category":"Classification","dataset_id":2,"train_config":"{\"epochs\":1}"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(r.Body, &got)
	if r.String() != "POST /train/config" || got["dataset_id"] != float64(2) || got["export_config"] != nil ||
		got["train_config"].(map[string]any)["epochs"] != float64(1) {
		t.Fatalf("Körper: %s", r.Body)
	}
	if _, present := got["export_config"]; !present {
		t.Fatal("export_config muss als null mitgehen (Backend liest es mit itemgetter)")
	}
	if !tool.Write {
		t.Fatal("create_training muss als schreibend gelten")
	}
}

func TestToolSchemas(t *testing.T) {
	seen := map[string]string{}
	for _, tool := range Tools {
		for _, n := range []string{tool.Name, tool.CLI} {
			if o, ok := seen[n]; ok && o != tool.Name {
				t.Fatalf("Name doppelt: %s (%s, %s)", n, o, tool.Name)
			}
			seen[n] = tool.Name
		}
		s := tool.Schema()
		if s["type"] != "object" {
			t.Fatalf("%s: Schema ohne type object", tool.Name)
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatal(err)
		}
	}
}

// testdata/openapi.json ist die OpenAPI-Beschreibung der Instanz vom 05.10.2026.
func TestOpenAPIDigest(t *testing.T) {
	raw, err := os.ReadFile("testdata/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	all, err := openAPIDigest(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) > DefaultMaxResult || !strings.Contains(string(all), "(130 Operationen") {
		t.Fatalf("Verzeichnis zu groß oder unvollständig: %d Bytes", len(all))
	}
	if !strings.Contains(string(all), "POST /datasets — Create Dataset · Körper: multipart/form-data") {
		t.Fatalf("Zeile für POST /datasets fehlt:\n%s", all)
	}
	train, _ := openAPIDigest(raw, "/train")
	if strings.Contains(string(train), "/datasets") || !strings.Contains(string(train), "GET /train/containers/{train_container_id}/logs — Get Train Container Logs · query: tail, max_length") {
		t.Fatalf("Filter /train:\n%s", train)
	}
	t.Logf("Verzeichnis: %d Bytes", len(all))
}

func TestDigestAppliedOnlyOnSuccess(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"paths":{"/a":{"get":{"summary":"A"}}}}`)
	}}
	c, _ := newClient(t, f, 0)
	tool, _ := Lookup("api_paths")
	req, err := tool.Build(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(context.Background(), "", req)
	if err != nil || !strings.HasPrefix(res.Body, "GET /a — A") {
		t.Fatalf("Verdichtung: %+v %v", res, err)
	}
}

// Review K1: GETs, die im Backend schreiben, brauchen eine Bestätigung.
func TestWritingGETs(t *testing.T) {
	cases := []struct {
		r    Request
		want bool
	}{
		{Request{Method: "GET", Path: "/train/containers/3/model"}, true},
		{Request{Method: "GET", Path: "/train/containers/3"}, false},
		{Request{Method: "GET", Path: "/licenses/"}, false},
		{Request{Method: "GET", Path: "/licenses/", Query: map[string]string{"return_cached": "false"}}, true},
		{Request{Method: "GET", Path: "/datasets"}, false},
		{Request{Method: "DELETE", Path: "/datasets/1"}, true},
	}
	for _, c := range cases {
		if c.r.Writes() != c.want {
			t.Errorf("%s: Writes()=%v", c.r, !c.want)
		}
	}
	tool, _ := Lookup("request")
	r, err := tool.Build(json.RawMessage(`{"method":"get","path":"/train/containers/3/model"}`))
	if err != nil || !r.Writes() {
		t.Fatalf("Rohzugriff auf schreibendes GET: %+v %v", r, err)
	}
	// Doppelter Schlüssel: der letzte gilt, und genau der wird geprüft.
	r, err = tool.Build(json.RawMessage(`{"method":"GET","method":"POST","path":"/datasets"}`))
	if err != nil || r.Method != "POST" || !r.Writes() {
		t.Fatalf("doppelte Methode: %+v %v", r, err)
	}
}

// Review K2: Geheimnisse kommen nie beim Agenten an.
func TestRedactAndNetworkBlocked(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":1,"edge_key":"EK","nested":{"Password":"pw","api_key":"AK","name":"x"},"big":12345678901234567890,"git_access_token":null}]`)
	}}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/edge-devices"})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"EK", `"pw"`, "AK"} {
		if strings.Contains(res.Body, leak) {
			t.Fatalf("Geheimnis %s im Ergebnis: %s", leak, res.Body)
		}
	}
	if !strings.Contains(res.Body, "12345678901234567890") || !strings.Contains(res.Body, `"name":"x"`) || !strings.Contains(res.Body, `"git_access_token":null`) {
		t.Fatalf("zu viel geschwärzt oder Zahl verfälscht: %s", res.Body)
	}
	for _, p := range []string{"/network", "/network/info", "/Network/1"} {
		if _, err := Normalize(Request{Method: "GET", Path: p}); err == nil {
			t.Errorf("%s muss gesperrt sein", p)
		}
	}
}

// Review W2: Do prüft selbst, auch ohne vorheriges Normalize.
func TestDoNormalizes(t *testing.T) {
	f := &fakePlatform{}
	c, _ := newClient(t, f, 0)
	res, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/urls/basic-auth"})
	if err != nil || res.Status != "error" || len(f.reqs) != 0 {
		t.Fatalf("gesperrter Pfad an Do vorbei: %+v %v %v", res, err, f.reqs)
	}
}

// Review W3: Binärantworten gehen nicht in den Kontext.
func TestBinaryResponse(t *testing.T) {
	f := &fakePlatform{respond: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write([]byte("PK\x03\x04\x00\x01binär"))
	}}
	c, _ := newClient(t, f, 0)
	res, _ := c.Do(context.Background(), "", Request{Method: "GET", Path: "/datasets/1/download"})
	if !strings.HasPrefix(res.Body, "[Binärantwort, ") || strings.Contains(res.Body, "PK") {
		t.Fatalf("Binärantwort: %q", res.Body)
	}
}

// Review W1: zu lange Abfragen werden abgewiesen statt in der Bestätigung gekürzt.
func TestQueryLimits(t *testing.T) {
	if _, err := Normalize(Request{Method: "GET", Path: "/x", Query: map[string]string{"a": strings.Repeat("x", 2000)}}); err == nil {
		t.Fatal("langer Wert erwartet abgewiesen")
	}
	q := map[string]string{}
	for i := range 10 {
		q[fmt.Sprint("k", i)] = strings.Repeat("x", 900)
	}
	if _, err := Normalize(Request{Method: "GET", Path: "/x", Query: q}); err == nil {
		t.Fatal("lange Abfrage erwartet abgewiesen")
	}
}

// Review M3: Protokolle standardmäßig vom Ende, tail positiv.
func TestTrainingLogsTail(t *testing.T) {
	tool, _ := Lookup("training_logs")
	r, err := tool.Build(json.RawMessage(`{"train_container_id":2}`))
	if err != nil || r.String() != "GET /train/containers/2/logs?tail=200" {
		t.Fatalf("Standard: %s %v", r, err)
	}
	if _, err := tool.Build(json.RawMessage(`{"train_container_id":2,"tail":-1}`)); err == nil {
		t.Fatal("negativer tail erwartet abgewiesen")
	}
}

// Review W4: Der Token-Endpunkt darf das Passwort nicht weiterleiten.
func TestTokenNoRedirect(t *testing.T) {
	var hit bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", User: "u", Password: "geheim"})
	if _, err := c.Do(context.Background(), "", Request{Method: "GET", Path: "/datasets"}); err == nil {
		t.Fatal("Anmeldung über Weiterleitung erwartet fehlgeschlagen")
	}
	if hit {
		t.Fatal("Passwort an das Ziel der Weiterleitung geschickt")
	}
}
