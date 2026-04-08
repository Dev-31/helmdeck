package placeholder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/store"
	"github.com/tosin2013/helmdeck/internal/vault"
)

// Builds an in-memory vault with two credentials granted to the
// "*" actor: an api_key named "github-token" and a login named
// "site-pw".
func newTestVault(t *testing.T) *vault.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key := make([]byte, 32)
	v, err := vault.New(db, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []vault.CreateInput{
		{Name: "github-token", Type: vault.TypeAPIKey, HostPattern: "api.github.com", Plaintext: []byte("ghp_secret_value")},
		{Name: "site-pw", Type: vault.TypeLogin, HostPattern: "site.example", Plaintext: []byte("hunter2")},
	} {
		rec, err := v.Create(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Grant(context.Background(), rec.ID, vault.Grant{ActorSubject: "*"}); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func TestSubstitute_NoPlaceholderUnchanged(t *testing.T) {
	r := New(newTestVault(t), nil)
	out, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"}, "plain string")
	if err != nil {
		t.Fatal(err)
	}
	if out != "plain string" {
		t.Errorf("got %q", out)
	}
}

func TestSubstitute_SinglePlaceholder(t *testing.T) {
	r := New(newTestVault(t), nil)
	out, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"},
		"Bearer ${vault:github-token}")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Bearer ghp_secret_value" {
		t.Errorf("got %q", out)
	}
}

func TestSubstitute_MultiplePlaceholders(t *testing.T) {
	r := New(newTestVault(t), nil)
	out, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"},
		"u=${vault:site-pw}&t=${vault:github-token}&u2=${vault:site-pw}")
	if err != nil {
		t.Fatal(err)
	}
	if out != "u=hunter2&t=ghp_secret_value&u2=hunter2" {
		t.Errorf("got %q", out)
	}
}

func TestSubstitute_UnknownNameErrors(t *testing.T) {
	r := New(newTestVault(t), nil)
	_, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"},
		"${vault:no-such-cred}")
	if !errors.Is(err, ErrUnknownPlaceholder) {
		t.Errorf("expected ErrUnknownPlaceholder, got %v", err)
	}
}

func TestSubstitute_DeniedActorErrors(t *testing.T) {
	// Build a vault with one credential that has NO grants.
	db, _ := store.Open(":memory:")
	t.Cleanup(func() { db.Close() })
	v, _ := vault.New(db, make([]byte, 32))
	_, _ = v.Create(context.Background(), vault.CreateInput{
		Name: "private-key", Type: vault.TypeAPIKey, HostPattern: "h", Plaintext: []byte("nope"),
	})
	r := New(v, nil)
	_, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"},
		"${vault:private-key}")
	if !errors.Is(err, ErrDenied) {
		t.Errorf("expected ErrDenied, got %v", err)
	}
}

func TestSubstitute_NilResolverNoOp(t *testing.T) {
	var r *Resolver
	out, err := r.Substitute(context.Background(), vault.Actor{}, "${vault:x}")
	if err != nil || out != "${vault:x}" {
		t.Errorf("nil resolver should pass through, got out=%q err=%v", out, err)
	}
}

func TestSubstitute_NameCharsetTerminates(t *testing.T) {
	// `${vault:foo} bar` and `${vault:foo!bar}` — the first is a
	// valid placeholder followed by " bar", the second contains a
	// `!` which isn't in the name charset so the whole match
	// should fail to parse and be left untouched.
	r := New(newTestVault(t), nil)
	out, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"},
		"x=${vault:github-token}!suffix")
	if err != nil {
		t.Fatal(err)
	}
	if out != "x=ghp_secret_value!suffix" {
		t.Errorf("got %q", out)
	}
	// Pattern with invalid char inside braces stays literal.
	out2, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"},
		"${vault:invalid name with space}")
	if err != nil {
		t.Fatal(err)
	}
	if out2 != "${vault:invalid name with space}" {
		t.Errorf("invalid name should pass through, got %q", out2)
	}
}

func TestHTTPClient_RewritesAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	r := New(newTestVault(t), nil)
	client := r.HTTPClient(vault.Actor{Subject: "alice"})

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer ${vault:github-token}")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer ghp_secret_value" {
		t.Errorf("server saw Authorization=%q, want substituted bearer", gotAuth)
	}
}

func TestHTTPClient_URLSubstitutionIsCallerResponsibility(t *testing.T) {
	// Per the package contract, URL substitution happens BEFORE
	// the URL is parsed by net/url — pack handlers call
	// Substitute() on the raw URL string and only then construct
	// the http.Request. This test demonstrates the documented
	// pattern.
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
	}))
	defer srv.Close()

	r := New(newTestVault(t), nil)
	rawURL := srv.URL + "/path?token=${vault:github-token}"
	resolved, err := r.Substitute(context.Background(), vault.Actor{Subject: "alice"}, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", resolved, nil)
	client := r.HTTPClient(vault.Actor{Subject: "alice"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(gotQuery, "ghp_secret_value") {
		t.Errorf("query not substituted: %s", gotQuery)
	}
}

func TestHTTPClient_RewritesBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	r := New(newTestVault(t), nil)
	client := r.HTTPClient(vault.Actor{Subject: "alice"})

	body := strings.NewReader(`{"token":"${vault:github-token}","u":"alice"}`)
	req, _ := http.NewRequest("POST", srv.URL, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotBody != `{"token":"ghp_secret_value","u":"alice"}` {
		t.Errorf("body not substituted: %s", gotBody)
	}
}

func TestHTTPClient_UnknownPlaceholderFailsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when placeholder substitution fails")
	}))
	defer srv.Close()

	r := New(newTestVault(t), nil)
	client := r.HTTPClient(vault.Actor{Subject: "alice"})

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer ${vault:no-such}")
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected error from unknown placeholder")
	}
	if !strings.Contains(err.Error(), "no-such") {
		t.Errorf("error should mention the missing name: %v", err)
	}
}

func TestVault_GetByName(t *testing.T) {
	v := newTestVault(t)
	rec, err := v.GetByName(context.Background(), "github-token")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "github-token" || rec.Type != vault.TypeAPIKey {
		t.Errorf("got %+v", rec)
	}
	if _, err := v.GetByName(context.Background(), "no-such"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing name, got %v", err)
	}
}

func TestVault_ResolveByName_DeniedWithoutGrant(t *testing.T) {
	db, _ := store.Open(":memory:")
	t.Cleanup(func() { db.Close() })
	v, _ := vault.New(db, make([]byte, 32))
	_, _ = v.Create(context.Background(), vault.CreateInput{
		Name: "x", Type: vault.TypeAPIKey, HostPattern: "h", Plaintext: []byte("p"),
	})
	_, err := v.ResolveByName(context.Background(), vault.Actor{Subject: "alice"}, "x")
	if !errors.Is(err, vault.ErrDenied) {
		t.Errorf("expected ErrDenied, got %v", err)
	}
}
