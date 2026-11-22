package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
)

const destinationPath = "/api/v1/notifications/destination"

type destinationResponse struct {
	Destination *struct {
		Kind    string `json:"kind"`
		Topic   string `json:"topic"`
		URL     string `json:"url"`
		Format  string `json:"format"`
		Enabled bool   `json:"enabled"`
	} `json:"destination"`
	NtfyAvailable bool   `json:"ntfy_available"`
	NtfyBaseURL   string `json:"ntfy_base_url"`
	LastDelivery  *struct {
		At    *time.Time `json:"at"`
		Error string     `json:"error"`
	} `json:"last_delivery"`
}

func decodeDestination(t *testing.T, rec *httptest.ResponseRecorder) destinationResponse {
	t.Helper()
	var out destinationResponse

	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func newNotificationFixture(t *testing.T, ntfyBase string) *authFixture {
	t.Helper()
	return newAuthFixture(t, func(c *config.Config) { c.Availability.UserNtfyBaseURL = ntfyBase })
}

func TestMemberManagesOwnNtfyDestination(t *testing.T) {
	var f = newNotificationFixture(t, "https://ntfy.example")
	var alice = f.login(t, "alice")
	var rec = f.member(t, alice, http.MethodGet, destinationPath, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, %s", rec.Code, rec.Body.String())
	}
	if got := decodeDestination(t, rec); got.Destination != nil || !got.NtfyAvailable || got.NtfyBaseURL != "https://ntfy.example" || got.LastDelivery != nil {
		t.Fatalf("empty GET = %+v", got)
	}
	rec = f.member(t, alice, http.MethodPut, destinationPath, map[string]any{"kind": "ntfy", "topic": "alice_Topic-1", "enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, %s", rec.Code, rec.Body.String())
	}
	if got := decodeDestination(t, rec); got.Destination == nil || got.Destination.Topic != "alice_Topic-1" || !got.Destination.Enabled || got.Destination.Kind != "ntfy" {
		t.Fatalf("PUT = %+v", got)
	}
	if rec = f.member(t, f.login(t, "bob"), http.MethodGet, destinationPath, nil); decodeDestination(t, rec).Destination != nil {
		t.Error("bob can see alice's destination")
	}
	if rec = f.member(t, alice, http.MethodDelete, destinationPath, nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, %s", rec.Code, rec.Body.String())
	}
	if rec = f.member(t, alice, http.MethodGet, destinationPath, nil); decodeDestination(t, rec).Destination != nil {
		t.Error("destination survived DELETE")
	}
	if rec = f.member(t, alice, http.MethodDelete, destinationPath, nil); rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", rec.Code)
	}
}

func TestMemberCannotSetWebhookOrActAsAnotherUser(t *testing.T) {
	var f = newNotificationFixture(t, "https://ntfy.example")
	var alice = f.login(t, "alice")

	if rec := f.member(t, alice, http.MethodPut, destinationPath, map[string]any{"kind": "webhook", "url": "https://example.test/hook", "enabled": true}); rec.Code != http.StatusForbidden {
		t.Errorf("member webhook PUT = %d, want 403", rec.Code)
	}
	if rec := f.member(t, alice, http.MethodGet, destinationPath+"?server_id=server&user_id=bob", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member GET for bob = %d, want 403", rec.Code)
	}
	if rec := f.member(t, alice, http.MethodPut, destinationPath, map[string]any{"user_id": "bob", "kind": "ntfy", "topic": "bobs", "enabled": true}); rec.Code != http.StatusForbidden {
		t.Errorf("member PUT for bob = %d, want 403", rec.Code)
	}
	if rec := f.member(t, alice, http.MethodDelete, destinationPath+"?user_id=bob", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member DELETE for bob = %d, want 403", rec.Code)
	}
	if rec := f.member(t, alice, http.MethodPost, destinationPath+"/test?user_id=bob", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member test for bob = %d, want 403", rec.Code)
	}
}

func TestOperatorSetsWebhookForAUser(t *testing.T) {
	var f = newNotificationFixture(t, "")
	var rec = f.do(t, call{method: http.MethodPut, path: destinationPath, bearer: true, body: map[string]any{
		"server_id": "server", "user_id": "alice", "kind": "webhook", "url": "https://example.test/hook", "format": "ntfy", "enabled": true}})

	if rec.Code != http.StatusOK {
		t.Fatalf("operator PUT = %d, %s", rec.Code, rec.Body.String())
	}
	if got := decodeDestination(t, rec); got.Destination == nil || got.Destination.URL != "https://example.test/hook" || got.Destination.Format != "ntfy" || got.NtfyAvailable {
		t.Fatalf("operator PUT = %+v", got)
	}
	if rec = f.do(t, call{method: http.MethodGet, path: destinationPath, bearer: true}); rec.Code != http.StatusBadRequest {
		t.Errorf("operator GET without user = %d, want 400", rec.Code)
	}
	for _, body := range []map[string]any{
		{"server_id": "server", "user_id": "alice", "kind": "webhook", "url": "ftp://example.test", "enabled": true},
		{"server_id": "server", "user_id": "alice", "kind": "webhook", "url": "https://u:p@example.test", "enabled": true},
		{"server_id": "server", "user_id": "alice", "kind": "webhook", "url": "https://example.test/#x", "enabled": true},
		{"server_id": "server", "user_id": "alice", "kind": "webhook", "url": "https://example.test", "format": "xml", "enabled": true},
		{"server_id": "server", "user_id": "nobody", "kind": "ntfy", "topic": "t", "enabled": true},
	} {
		if rec = f.do(t, call{method: http.MethodPut, path: destinationPath, bearer: true, body: body}); rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("PUT %v = %d, want 400 or 404", body, rec.Code)
		}
	}
}

func TestDestinationValidation(t *testing.T) {
	var f = newNotificationFixture(t, "https://ntfy.example")
	var alice = f.login(t, "alice")

	for _, topic := range []string{"", "has space", "slash/topic", "dots.not.ok", strings.Repeat("a", 65), "emoji☃"} {
		if rec := f.member(t, alice, http.MethodPut, destinationPath, map[string]any{"kind": "ntfy", "topic": topic, "enabled": true}); rec.Code != http.StatusBadRequest {
			t.Errorf("topic %q = %d, want 400", topic, rec.Code)
		}
	}
	if rec := f.member(t, alice, http.MethodPut, destinationPath, map[string]any{"kind": "ntfy", "topic": strings.Repeat("a", 64), "enabled": true}); rec.Code != http.StatusOK {
		t.Errorf("64-character topic = %d, want 200", rec.Code)
	}
	if rec := f.member(t, alice, http.MethodPut, destinationPath, map[string]any{"kind": "sms", "enabled": true}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown kind = %d, want 400", rec.Code)
	}
}

func TestNtfyDestinationConflictsWithoutBaseURL(t *testing.T) {
	var f = newNotificationFixture(t, "")
	var alice = f.login(t, "alice")

	if rec := f.member(t, alice, http.MethodPut, destinationPath, map[string]any{"kind": "ntfy", "topic": "topic", "enabled": true}); rec.Code != http.StatusConflict {
		t.Errorf("PUT without base URL = %d, want 409", rec.Code)
	}
	if got := decodeDestination(t, f.member(t, alice, http.MethodGet, destinationPath, nil)); got.NtfyAvailable {
		t.Error("ntfy_available true without a base URL")
	}
}

func TestTestNotificationSuccessFailureAndRateLimit(t *testing.T) {
	var gotPath, gotBody string
	var status = http.StatusNoContent
	var ntfy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body, _ = io.ReadAll(r.Body)

		gotPath, gotBody = r.URL.Path, string(body)
		w.WriteHeader(status)
	}))
	defer ntfy.Close()
	var f = newNotificationFixture(t, ntfy.URL)
	var alice, bob = f.login(t, "alice"), f.login(t, "bob")
	var testPath = destinationPath + "/test"

	if rec := f.member(t, alice, http.MethodPost, testPath, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("test without destination = %d, want 404", rec.Code)
	}
	for name, token := range map[string]string{"alice": alice, "bob": bob} {
		if rec := f.member(t, token, http.MethodPut, destinationPath, map[string]any{"kind": "ntfy", "topic": "topic-" + name, "enabled": true}); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d, %s", rec.Code, rec.Body.String())
		}
	}
	rec := f.member(t, alice, http.MethodPost, testPath, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"delivered":true`) {
		t.Fatalf("test = %d, %s", rec.Code, rec.Body.String())
	}
	if gotBody != "Reelay test notification" || !strings.HasPrefix(gotPath, "/topic-") {
		t.Errorf("receiver got %s %q", gotPath, gotBody)
	}
	rec = f.member(t, alice, http.MethodPost, testPath, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("second test = %d retry-after %q, want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	status = http.StatusInternalServerError
	rec = f.member(t, bob, http.MethodPost, testPath, nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "webhook returned HTTP 500") {
		t.Errorf("failing receiver = %d, %s, want 502 with the delivery error", rec.Code, rec.Body.String())
	}
}

func TestTestNotificationLimiterIsBounded(t *testing.T) {
	var l = newTestNotificationLimiter()
	var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3*testNotificationsCap; i++ {
		l.take(strconv.Itoa(i), now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(l.last) > testNotificationsCap {
		t.Errorf("limiter holds %d entries, cap %d", len(l.last), testNotificationsCap)
	}
	if wait := l.take("fresh", now.Add(time.Hour)); wait != 0 {
		t.Errorf("wait after expiry = %v", wait)
	}
}

func TestMetricsExposeUserNotificationsPending(t *testing.T) {
	var s, _ = newTestServer(t, nil)
	var rec = do(t, s.handler(), http.MethodGet, "/api/v1/metrics", authed())

	if !strings.Contains(rec.Body.String(), "reelay_user_notifications_pending 0\n") {
		t.Errorf("metrics missing reelay_user_notifications_pending:\n%s", rec.Body.String())
	}
}
