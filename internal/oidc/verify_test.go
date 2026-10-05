package oidc

import (
	"context"
	"strings"
	"testing"
	"time"

	"agw/internal/oidc/oidctest"
)

func testService(t *testing.T, is *oidctest.Issuer) *Service {
	t.Helper()
	s, err := New(Config{Issuer: is.IssuerURL(), ClientID: is.ClientID, ClientSecret: is.ClientSecret, PublicURL: "http://localhost:1"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerify(t *testing.T) {
	is := oidctest.New(t)
	s := testService(t, is)
	ctx := context.Background()
	u := oidctest.User{Sub: "s1", Username: "anna"}
	good := is.AccessClaims(u)
	if c, err := s.verify(ctx, is.Sign(good)); err != nil || c.Sub != "s1" || c.Azp != "agw-agent" {
		t.Fatalf("gültig: %+v %v", c, err)
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range good {
			m[a] = b
		}
		m[k] = v
		return m
	}
	none := strings.Join([]string{"eyJhbGciOiJub25lIn0", strings.Split(is.Sign(good), ".")[1], ""}, ".")
	for name, tok := range map[string]string{
		"fremder Schlüssel": is.SignForeign(good),
		"fremder Issuer":    is.Sign(with("iss", "https://evil.example/realms/test")),
		"abgelaufen":        is.Sign(with("exp", time.Now().Add(-time.Hour).Unix())),
		"ohne exp":          is.Sign(with("exp", 0)),
		"alg none":          none,
		"kein JWT":          "abc",
	} {
		if _, err := s.verify(ctx, tok); err == nil {
			t.Errorf("%s: angenommen", name)
		}
	}
}

func TestCheckLogin(t *testing.T) {
	is := oidctest.New(t)
	s := testService(t, is)
	ctx := context.Background()
	u := oidctest.User{Sub: "s1", Username: "anna", Name: "Anna"}
	id := func(mod func(map[string]any)) string {
		c := map[string]any{"iss": is.IssuerURL(), "sub": "s1", "aud": "agw-agent", "azp": "agw-agent", "nonce": "n1",
			"preferred_username": "anna", "name": "Anna", "exp": time.Now().Add(time.Minute).Unix()}
		if mod != nil {
			mod(c)
		}
		return is.Sign(c)
	}
	access := is.Sign(is.AccessClaims(u))
	if got, _, err := s.checkLogin(ctx, tokenResp{ID: id(nil), Access: access}, "n1"); err != nil || got.Username != "anna" || got.Name != "Anna" {
		t.Fatalf("gültig: %+v %v", got, err)
	}
	otherClient := is.AccessClaims(u)
	otherClient["azp"] = "frontend"
	otherSub := is.AccessClaims(oidctest.User{Sub: "s2"})
	for name, tr := range map[string]tokenResp{
		"falsche nonce":          {ID: id(func(c map[string]any) { c["nonce"] = "n2" }), Access: access},
		"fremde Zielgruppe":      {ID: id(func(c map[string]any) { c["aud"] = "frontend"; c["azp"] = "frontend" }), Access: access},
		"ohne ID-Token":          {Access: access},
		"Token für frontend":     {ID: id(nil), Access: is.Sign(otherClient)},
		"Token anderer Nutzer":   {ID: id(nil), Access: is.Sign(otherSub)},
		"Zugangstoken gefälscht": {ID: id(nil), Access: is.SignForeign(is.AccessClaims(u))},
	} {
		if _, _, err := s.checkLogin(ctx, tr, "n1"); err == nil {
			t.Errorf("%s: angenommen", name)
		}
	}
}
