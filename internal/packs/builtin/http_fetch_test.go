package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/security"
	"github.com/tosin2013/helmdeck/internal/store"
	"github.com/tosin2013/helmdeck/internal/vault"
)

// vaultWithCreds builds an in-memory vault and seeds it with the
// caller's credential list. Each credential is granted to "*" so
// tests don't have to micromanage ACL.
func vaultWithCreds(t *testing.T, creds ...vault.CreateInput) *vault.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	v, err := vault.New(db, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range creds {
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

// allowingEgressGuard returns a guard whose stub resolver maps every
// host to the loopback IP of the given test server, then allowlists
// 127.0.0.0/8 so the guard accepts the lookup. This is the
// equivalent of "egress guard installed but configured to permit
// the test target" — closer to production than passing nil.
func allowingEgressGuard(addr string) *security.EgressGuard {
	host, _, _ := net.SplitHostPort(addr)
	return security.New(
		security.WithResolver(stubFixedResolver{ip: host}),
		security.WithAllowlist([]string{"127.0.0.0/8"}),
	)
}

type stubFixedResolver struct{ ip string }

func (s stubFixedResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP(s.ip)}}, nil
}

func TestHTTPFetch_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_secret" {
			t.Errorf("server saw Authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	v := vaultWithCreds(t, vault.CreateInput{
		Name: "github-token", Type: vault.TypeAPIKey, HostPattern: "*", Plaintext: []byte("ghp_secret"),
	})
	eng := packs.New()
	pack := HTTPFetch(v, allowingEgressGuard(srv.Listener.Addr().String()))

	body := `{
	  "url":     "` + srv.URL + `/v1/repos",
	  "method":  "GET",
	  "headers": {"Authorization": "Bearer ${vault:github-token}"}
	}`
	res, err := eng.Execute(context.Background(), pack, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Status != 200 || out.Body != `{"ok":true}` {
		t.Errorf("output wrong: %+v", out)
	}
}

func TestHTTPFetch_PlaceholderInURL(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
	}))
	defer srv.Close()

	v := vaultWithCreds(t, vault.CreateInput{
		Name: "key", Type: vault.TypeAPIKey, HostPattern: "*", Plaintext: []byte("topsecret"),
	})
	eng := packs.New()
	pack := HTTPFetch(v, allowingEgressGuard(srv.Listener.Addr().String()))

	body := `{"url":"` + srv.URL + `/path?token=${vault:key}"}`
	if _, err := eng.Execute(context.Background(), pack, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "topsecret") {
		t.Errorf("URL substitution didn't reach the server: %s", gotQuery)
	}
}

func TestHTTPFetch_PlaceholderInBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(201)
	}))
	defer srv.Close()

	v := vaultWithCreds(t, vault.CreateInput{
		Name: "tok", Type: vault.TypeAPIKey, HostPattern: "*", Plaintext: []byte("v"),
	})
	eng := packs.New()
	pack := HTTPFetch(v, allowingEgressGuard(srv.Listener.Addr().String()))

	body := `{
	  "url":    "` + srv.URL + `",
	  "method": "POST",
	  "body":   "{\"token\":\"${vault:tok}\"}"
	}`
	if _, err := eng.Execute(context.Background(), pack, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"token":"v"}` {
		t.Errorf("body substitution wrong: %s", gotBody)
	}
}

func TestHTTPFetch_UnknownPlaceholderRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when placeholder substitution fails")
	}))
	defer srv.Close()

	v := vaultWithCreds(t)
	eng := packs.New()
	pack := HTTPFetch(v, allowingEgressGuard(srv.Listener.Addr().String()))

	body := `{
	  "url":     "` + srv.URL + `",
	  "headers": {"Authorization": "Bearer ${vault:no-such}"}
	}`
	_, err := eng.Execute(context.Background(), pack, json.RawMessage(body))
	if err == nil {
		t.Fatal("expected unknown placeholder to fail")
	}
	pe, _ := err.(*packs.PackError)
	if pe == nil || pe.Code != packs.CodeInvalidInput {
		t.Errorf("expected invalid_input, got %v", err)
	}
}

func TestHTTPFetch_EgressGuardBlocksMetadataIP(t *testing.T) {
	v := vaultWithCreds(t)
	eng := packs.New()
	// A guard with no allowlist + a stub resolver that returns the
	// metadata IP. The pack must short-circuit before any HTTP call.
	guard := security.New(
		security.WithResolver(stubFixedResolver{ip: "169.254.169.254"}),
	)
	pack := HTTPFetch(v, guard)
	body := `{"url":"https://meta.example/"}`
	_, err := eng.Execute(context.Background(), pack, json.RawMessage(body))
	if err == nil {
		t.Fatal("expected egress guard to block metadata host")
	}
	if !strings.Contains(err.Error(), "egress denied") {
		t.Errorf("error should mention egress: %v", err)
	}
}

func TestHTTPFetch_RejectsExoticMethod(t *testing.T) {
	v := vaultWithCreds(t)
	eng := packs.New()
	pack := HTTPFetch(v, nil)
	_, err := eng.Execute(context.Background(), pack,
		json.RawMessage(`{"url":"https://example.com","method":"CONNECT"}`))
	if err == nil {
		t.Fatal("expected CONNECT to be rejected")
	}
}

func TestHTTPFetch_DefaultsMethodToGET(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(200)
	}))
	defer srv.Close()
	v := vaultWithCreds(t)
	eng := packs.New()
	pack := HTTPFetch(v, allowingEgressGuard(srv.Listener.Addr().String()))
	if _, err := eng.Execute(context.Background(), pack,
		json.RawMessage(`{"url":"`+srv.URL+`"}`)); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "GET" {
		t.Errorf("default method should be GET, got %s", gotMethod)
	}
}

func TestHTTPFetch_RequiresURL(t *testing.T) {
	eng := packs.New()
	pack := HTTPFetch(vaultWithCreds(t), nil)
	_, err := eng.Execute(context.Background(), pack, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestHTTPFetch_NonOKStatusIsNotPackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()
	v := vaultWithCreds(t)
	eng := packs.New()
	pack := HTTPFetch(v, allowingEgressGuard(srv.Listener.Addr().String()))
	res, err := eng.Execute(context.Background(), pack,
		json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("4xx should not be a pack error: %v", err)
	}
	var out struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Status != 404 || out.Body != "not found" {
		t.Errorf("output wrong: %+v", out)
	}
}
