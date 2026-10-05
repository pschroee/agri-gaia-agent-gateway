package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agw/internal/oidc/oidctest"
)

var errNoSession = errors.New("Anmeldung des Nutzers abgelaufen; Chat in der Plattform öffnen")

// subjectSetup: nachgebildeter Realm (Token-Austausch prüft die Signatur des subject_token) und eine
// API, die den sub jedes eingehenden Tokens festhält. Chats gehören Nutzern laut owners; Nutzer ohne
// Eintrag in tokens haben keine Sitzung.
func subjectSetup(t *testing.T, owners map[string]string, live map[string]oidctest.User) (*Client, *oidctest.Issuer, func() []string) {
	t.Helper()
	is := oidctest.New(t)
	var mu sync.Mutex
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cl, err := ParseClaims(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if err != nil || cl.Azp != "agw-agent" || !strings.Contains(strings.Join(cl.Aud, ","), "backend") {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		seen = append(seen, cl.Sub)
		mu.Unlock()
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(api.Close)
	subject := func(ctx context.Context, chatID string) (string, error) {
		u, ok := live[owners[chatID]]
		if !ok {
			return "", errNoSession
		}
		return is.Sign(is.AccessClaims(u)), nil
	}
	c, err := New(Config{APIURL: api.URL, TokenURL: is.IssuerURL() + "/protocol/openid-connect/token", ClientID: is.ClientID,
		ClientSecret: is.ClientSecret, Exchange: true, Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	return c, is, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// Der Austausch nimmt das Token des Chat-Besitzers; zwei Chats zweier Nutzer bekommen verschiedene sub.
func TestSubjectFromChatOwner(t *testing.T) {
	anna := oidctest.User{Sub: "sub-anna", Username: "anna"}
	bert := oidctest.User{Sub: "sub-bert", Username: "bert"}
	c, is, seen := subjectSetup(t, map[string]string{"chat-a": "sub-anna", "chat-b": "sub-bert"},
		map[string]oidctest.User{"sub-anna": anna, "sub-bert": bert})
	var logged []string
	c.SetOnExchange(func(chatID string, cl Claims) { logged = append(logged, chatID+"="+cl.Sub) })
	ctx := context.Background()
	for _, chat := range []string{"chat-a", "chat-b", "chat-a"} {
		res, err := c.Do(ctx, chat, Request{Method: "GET", Path: "/datasets"})
		if err != nil || res.Status != "ok" {
			t.Fatalf("%s: %+v %v", chat, res, err)
		}
	}
	ex := is.Exchanges()
	if len(ex) != 2 || ex[0].SubjectSub != "sub-anna" || ex[1].SubjectSub != "sub-bert" {
		t.Fatalf("Austausche: %+v", ex)
	}
	if got := strings.Join(seen(), ","); got != "sub-anna,sub-bert,sub-anna" {
		t.Fatalf("an der API: %s", got)
	}
	if strings.Join(logged, ",") != "chat-a=sub-anna,chat-b=sub-bert" {
		t.Fatalf("Protokoll: %v", logged)
	}
}

// Ohne Sitzung des Besitzers scheitert der Aufruf mit klarer Meldung; getauscht wird nichts.
func TestSubjectMissingSession(t *testing.T) {
	c, is, seen := subjectSetup(t, map[string]string{"chat-a": "sub-anna"}, map[string]oidctest.User{})
	_, err := c.Do(context.Background(), "chat-a", Request{Method: "GET", Path: "/datasets"})
	if err == nil || !strings.Contains(err.Error(), "Anmeldung des Nutzers abgelaufen; Chat in der Plattform öffnen") {
		t.Fatalf("erwartet Fehler zur Anmeldung: %v", err)
	}
	if len(is.Exchanges()) != 0 || len(seen()) != 0 {
		t.Fatal("trotz fehlender Sitzung getauscht oder aufgerufen")
	}
}

// Mit Subject braucht New weder Nutzer noch Passwort.
func TestSubjectConfig(t *testing.T) {
	sub := func(context.Context, string) (string, error) { return "", nil }
	if _, err := New(Config{APIURL: "https://api.example", Subject: sub}); err != nil {
		t.Fatalf("ohne Austausch: %v", err)
	}
	if _, err := New(Config{APIURL: "https://api.example", Subject: sub, Exchange: true, ClientSecret: "s"}); err == nil {
		t.Fatal("Austausch ohne Token-URL angenommen")
	}
	if _, err := New(Config{APIURL: "https://api.example"}); err == nil {
		t.Fatal("ohne Subject und ohne Passwort angenommen")
	}
}
