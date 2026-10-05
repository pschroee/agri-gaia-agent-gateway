package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKeycloak bildet den Token-Endpunkt mit Passwort-Grant und Token-Austausch nach (wie Keycloak
// 26: kein act-Claim, azp = anfragender Client) und dahinter die API, die nur getauschte Tokens
// mit Zielgruppe backend annimmt.
type fakeKeycloak struct {
	mu        sync.Mutex
	n         int
	user      string // gültiges Nutzertoken
	exchanges []string
	apiTokens []string
	multipart []string
}

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(b) + ".sig"
}

func (f *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "agw-agent" || r.Form.Get("client_secret") != "geheim" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized_client"}`)
			return
		}
		f.n++
		switch r.Form.Get("grant_type") {
		case "password":
			f.user = jwt(map[string]any{"sub": "u-1", "preferred_username": "test", "azp": "agw-agent", "aud": "account", "jti": fmt.Sprint("u", f.n)})
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"r","expires_in":3600}`, f.user)
		case "urn:ietf:params:oauth:grant-type:token-exchange":
			if r.Form.Get("subject_token") != f.user || r.Form.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:access_token" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_token"}`)
				return
			}
			aud := r.Form["audience"]
			f.exchanges = append(f.exchanges, strings.Join(aud, ","))
			tok := jwt(map[string]any{"sub": "u-1", "preferred_username": "test", "azp": "agw-agent", "aud": aud, "exp": time.Now().Add(time.Hour).Unix(), "jti": fmt.Sprint("x", f.n)})
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, tok)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	cl, err := ParseClaims(tok)
	if err != nil || tok == f.user || cl.Azp != "agw-agent" || !strings.Contains(strings.Join(cl.Aud, ","), "backend") {
		w.WriteHeader(http.StatusUnauthorized) // nur getauschte Tokens, nie das Nutzertoken selbst
		return
	}
	f.apiTokens = append(f.apiTokens, cl.JTI)
	if mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				f.multipart = append(f.multipart, fmt.Sprintf("%s=@%s:%s", p.FormName(), p.FileName(), b))
			} else {
				f.multipart = append(f.multipart, fmt.Sprintf("%s=%s", p.FormName(), b))
			}
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":9}`)
		return
	}
	fmt.Fprint(w, `[]`)
}

func exchangeClient(t *testing.T, f *fakeKeycloak) (*Client, *[]string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "agw-agent", ClientSecret: "geheim", User: "test", Password: "pw", Exchange: true})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logged []string
	c.SetOnExchange(func(chat string, cl Claims) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, chat+": "+cl.String())
	})
	return c, &logged
}

func TestExchangePerChat(t *testing.T) {
	f := &fakeKeycloak{}
	c, logged := exchangeClient(t, f)
	ctx := context.Background()
	get := Request{Method: "GET", Path: "/datasets"}
	for _, chat := range []string{"chat-a", "chat-a", "chat-b"} {
		if res, err := c.Do(ctx, chat, get); err != nil || res.HTTPStatus != 200 {
			t.Fatalf("%s: %+v %v", chat, res, err)
		}
	}
	if len(f.exchanges) != 2 || f.exchanges[0] != "backend,minio" {
		t.Fatalf("je Chat genau ein Austausch mit backend,minio: %v", f.exchanges)
	}
	if f.apiTokens[0] != f.apiTokens[1] || f.apiTokens[1] == f.apiTokens[2] {
		t.Fatalf("Chat a nutzt sein Token weiter, Chat b ein eigenes: %v", f.apiTokens)
	}
	if len(*logged) != 2 || !strings.Contains((*logged)[0], "chat-a: Nutzer test, azp=agw-agent, aud=backend,minio") || !strings.Contains((*logged)[0], "ohne act (Impersonation)") {
		t.Fatalf("Protokoll des Austauschs: %v", *logged)
	}
	// Ruhen verwirft das Token, Fortsetzen tauscht neu.
	c.Forget("chat-a")
	_, _ = c.Do(ctx, "chat-a", get)
	if len(f.exchanges) != 3 {
		t.Fatalf("nach Forget kein neuer Austausch: %v", f.exchanges)
	}
	// Ohne Chat kein Aufruf: Das Nutzertoken selbst geht nie an die API.
	if _, err := c.Do(ctx, "", get); err == nil {
		t.Fatal("Aufruf ohne Chat erwartet abgewiesen")
	}
}

func TestExchangeMaxAgeAndUserRelogin(t *testing.T) {
	f := &fakeKeycloak{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, _ := New(Config{APIURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "agw-agent", ClientSecret: "geheim", User: "test", Password: "pw", Exchange: true, ChatTokenMaxAge: 40 * time.Second})
	ctx := context.Background()
	_, _ = c.Do(ctx, "c", Request{Method: "GET", Path: "/x"})
	_, _ = c.Do(ctx, "c", Request{Method: "GET", Path: "/x"}) // 40 s Höchstalter, 30 s Puffer: noch gültig
	if len(f.exchanges) != 1 {
		t.Fatalf("Höchstalter: %v", f.exchanges)
	}
	// Nutzertoken serverseitig verfallen: Austausch scheitert, neue Anmeldung, dann gelingt er.
	c.Forget("c")
	f.mu.Lock()
	f.user = "verfallen"
	f.mu.Unlock()
	if res, err := c.Do(ctx, "c", Request{Method: "GET", Path: "/x"}); err != nil || res.HTTPStatus != 200 {
		t.Fatalf("nach verfallenem Nutzertoken: %+v %v", res, err)
	}
}

func TestExchangeNeedsSecret(t *testing.T) {
	if _, err := New(Config{APIURL: "https://x", TokenURL: "https://x/t", User: "u", Password: "p", Exchange: true}); err == nil {
		t.Fatal("Austausch ohne Client-Secret erwartet abgewiesen")
	}
}

func TestMultipartUpload(t *testing.T) {
	f := &fakeKeycloak{}
	c, _ := exchangeClient(t, f)
	tool, _ := Lookup("upload_dataset")
	req, err := tool.Build(json.RawMessage(`{"name":"ferkel","description":"Bilder","files":["/workspace/a.png","/workspace/b.png"],"annotation_file":"/workspace/annotations.xml","annotation_labels":["0","1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.String() != "POST /datasets (multipart, 3 Dateien)" || !req.Writes() {
		t.Fatalf("Aufruf: %s", req)
	}
	if _, err := c.Do(context.Background(), "c", req); err == nil {
		// ohne gelesene Dateien darf nichts hinausgehen
		if len(f.multipart) != 0 {
			t.Fatal("Upload ohne gelesene Dateien gesendet")
		}
	}
	for _, fl := range req.Files {
		name := fl.Path[strings.LastIndex(fl.Path, "/")+1:]
		req.Uploads = append(req.Uploads, Upload{Field: fl.Field, Name: name, Data: []byte("inhalt-" + name), SHA256: "x"})
	}
	res, err := c.Do(context.Background(), "c", req)
	if err != nil || res.HTTPStatus != 201 {
		t.Fatalf("Upload: %+v %v", res, err)
	}
	got := strings.Join(f.multipart, ";")
	for _, want := range []string{"annotation_labels=0;annotation_labels=1", "includes_annotation_file=true", "is_classification_dataset=false", "metadata={}", "dataset_type=AgriImageDataResource",
		"files=@a.png:inhalt-a.png;files=@b.png:inhalt-b.png;files=@annotations.xml:inhalt-annotations.xml"} {
		if !strings.Contains(got, want) {
			t.Fatalf("multipart ohne %q: %s", want, got)
		}
	}
	if d := req.Describe(); !strings.Contains(d, "3 Dateien, zusammen") || !strings.Contains(d, "files: annotations.xml") || !strings.Contains(d, "name = ferkel") {
		t.Fatalf("Beschreibung für die Bestätigung: %s", d)
	}
}

func TestUploadToolsValidate(t *testing.T) {
	ds, _ := Lookup("upload_dataset")
	md, _ := Lookup("upload_model")
	bad := []struct {
		tool Tool
		args string
	}{
		{ds, `{"name":"n","description":"d","files":[]}`},
		{ds, `{"name":"n","description":"d","files":["relativ.png"]}`},
		{ds, `{"name":"n","description":"d","files":["/workspace/../etc/passwd"]}`},
		{ds, `{"name":"n","description":"d","files":[1]}`},
		{md, `{"name":"n","description":"d","format":"exe","model_file":"/workspace/m.onnx"}`},
		{md, `{"name":"n","description":"d","format":"onnx"}`},
		{md, `{"name":"n","description":"d","format":"onnx","model_file":"/workspace/m.onnx","keywords":["schwein"]}`},
	}
	for _, b := range bad {
		if r, err := b.tool.Build(json.RawMessage(b.args)); err == nil {
			t.Errorf("%s %s: erwartet Fehler, gebaut %s", b.tool.Name, b.args, r)
		}
	}
	r, err := md.Build(json.RawMessage(`{"name":"m","description":"d","format":"onnx","model_file":"/workspace/m.onnx","keywords":["http://aims.fao.org/aos/agrovoc/c_5714"]}`))
	if err != nil || r.Files[0].Field != "modelfile" || strings.Join(r.Form["labels"], ",") != "http://aims.fao.org/aos/agrovoc/c_5714" {
		t.Fatalf("upload_model: %+v %v", r, err)
	}
	// Formular nur mit POST/PUT/PATCH und nicht zusammen mit JSON-Körper.
	if _, err := Normalize(Request{Method: "GET", Path: "/datasets", Form: map[string][]string{"a": {"b"}}}); err == nil {
		t.Fatal("GET mit Formular erwartet abgewiesen")
	}
}
