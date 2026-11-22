package jellyfinauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type fakeJellyfin struct {
	srv        *httptest.Server
	loginBody  atomic.Value
	loginAuth  atomic.Value
	logoutAuth atomic.Value
	logouts    atomic.Int32
}

func newFakeJellyfin(t *testing.T, login func(w http.ResponseWriter)) *fakeJellyfin {
	t.Helper()
	f := &fakeJellyfin{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.loginBody.Store(string(raw))
		f.loginAuth.Store(r.Header.Get("Authorization"))
		login(w)
	})
	mux.HandleFunc("POST /Sessions/Logout", func(w http.ResponseWriter, r *http.Request) {
		f.logouts.Add(1)
		f.logoutAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeResult(w http.ResponseWriter, id string, admin bool) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"AccessToken": "jellyfin-session-token",
		"ServerId":    "0A1B2C3D-0000-0000-0000-000000000001",
		"User": map[string]any{
			"Id": id, "Name": "Alice",
			"Policy": map[string]any{"IsAdministrator": admin},
		},
	})
}

func testClient() *Client { return New(slog.New(slog.NewTextHandler(io.Discard, nil))) }

func TestAuthenticateMemberNormalizesIDAndLogsOut(t *testing.T) {
	f := newFakeJellyfin(t, func(w http.ResponseWriter) { writeResult(w, "ABCDEF01-2345-6789-ABCD-EF0123456789", false) })

	user, err := testClient().Authenticate(context.Background(), f.srv.URL+"/", "alice", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "abcdef0123456789abcdef0123456789" || user.Name != "Alice" || user.Administrator {
		t.Errorf("user = %+v", user)
	}
	if user.ServerID != "0a1b2c3d000000000000000000000001" {
		t.Errorf("server id = %q, want normalized from the top-level field", user.ServerID)
	}
	var sent map[string]string
	if err := json.Unmarshal([]byte(f.loginBody.Load().(string)), &sent); err != nil || sent["Username"] != "alice" || sent["Pw"] != "s3cret" {
		t.Errorf("login body = %v (%v)", f.loginBody.Load(), err)
	}
	if auth := f.loginAuth.Load().(string); !strings.HasPrefix(auth, `MediaBrowser Client="Reelay", Device="Reelay", DeviceId="reelay-login", Version="`) || strings.Contains(auth, "Token=") {
		t.Errorf("login Authorization = %q", auth)
	}
	if f.logouts.Load() != 1 {
		t.Fatalf("logout calls = %d, want 1", f.logouts.Load())
	}
	if auth := f.logoutAuth.Load().(string); !strings.Contains(auth, `Token="jellyfin-session-token"`) || !strings.HasPrefix(auth, "MediaBrowser ") {
		t.Errorf("logout Authorization = %q", auth)
	}
}

func TestAuthenticateReportsAdministrators(t *testing.T) {
	f := newFakeJellyfin(t, func(w http.ResponseWriter) { writeResult(w, "abcdef0123456789abcdef0123456789", true) })

	user, err := testClient().Authenticate(context.Background(), f.srv.URL, "admin", "pw")
	if err != nil || !user.Administrator {
		t.Fatalf("user = %+v, err = %v, want an administrator", user, err)
	}
}

func TestAuthenticateRejectsBadCredentials(t *testing.T) {
	f := newFakeJellyfin(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) })

	_, err := testClient().Authenticate(context.Background(), f.srv.URL, "alice", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if f.logouts.Load() != 0 {
		t.Errorf("logout called %d times without a session", f.logouts.Load())
	}
}

func TestAuthenticateServerErrorIsNotInvalidCredentials(t *testing.T) {
	f := newFakeJellyfin(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })

	_, err := testClient().Authenticate(context.Background(), f.srv.URL, "alice", "pw")
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want a non-credential failure", err)
	}
}

func TestAuthenticateRejectsUnusableUserID(t *testing.T) {
	f := newFakeJellyfin(t, func(w http.ResponseWriter) { writeResult(w, "not-a-guid", false) })

	if _, err := testClient().Authenticate(context.Background(), f.srv.URL, "alice", "pw"); err == nil {
		t.Fatal("unusable user id accepted")
	}
}

func TestAuthenticateCapsTheResponseBody(t *testing.T) {
	f := newFakeJellyfin(t, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"AccessToken":"t","User":{"Id":"abcdef0123456789abcdef0123456789","Name":"`)
		_, _ = io.WriteString(w, strings.Repeat("a", 2<<20))
		_, _ = io.WriteString(w, `"}}`)
	})

	if _, err := testClient().Authenticate(context.Background(), f.srv.URL, "alice", "pw"); err == nil {
		t.Fatal("a response beyond 1 MiB was accepted")
	}
}

func TestAuthenticateDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	if _, err := testClient().Authenticate(context.Background(), redirector.URL, "alice", "pw"); err == nil {
		t.Fatal("redirect treated as success")
	}
	if hits.Load() != 0 {
		t.Error("credentials were replayed to the redirect target")
	}
}
