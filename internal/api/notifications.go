package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/store"
)

const (
	maxTopicLen          = 64
	testNotificationGap  = time.Minute
	testNotificationsCap = 1024
	testPayload          = `{"event":"test","title":"Reelay test notification"}`
	testMessage          = "Reelay test notification"
)

type destinationBody struct {
	ServerID string `json:"server_id"`
	UserID   string `json:"user_id"`
	Kind     string `json:"kind"`
	Topic    string `json:"topic"`
	URL      string `json:"url"`
	Format   string `json:"format"`
	Enabled  bool   `json:"enabled"`
}

type lastDelivery struct {
	At       *time.Time `json:"at"`
	Error    string     `json:"error"`
	Attempts int        `json:"attempts"`
}

// notificationScope resolves the acting user (query parameters, or the body for
// operators' PUT) and requires them to be a synced Jellyfin user.
func (s *Server) notificationScope(r *http.Request, bodyServerID, bodyUserID string) (string, string, error) {
	serverID, userID := bodyServerID, bodyUserID

	if serverID == "" {
		serverID = r.URL.Query().Get("server_id")
	}
	if userID == "" {
		userID = r.URL.Query().Get("user_id")
	}
	serverID, userID, err := scopeFor(r, serverID, userID)
	if err != nil {
		return "", "", err
	}
	known, err := s.store.Recommendations().UserExists(r.Context(), serverID, userID)
	if err != nil {
		return "", "", err
	}
	if !known {
		return "", "", NotFound("Jellyfin user not found")
	}
	return serverID, userID, nil
}

func (s *Server) handleNotificationDestination(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := s.notificationScope(r, "", "")
	if err != nil {
		return err
	}
	return s.replyDestination(w, r, serverID, userID)
}

func (s *Server) replyDestination(w http.ResponseWriter, r *http.Request, serverID, userID string) error {
	var response = map[string]any{
		"destination":    nil,
		"ntfy_available": s.cfg.Availability.UserNtfyBaseURL != "",
		"ntfy_base_url":  s.cfg.Availability.UserNtfyBaseURL,
		"last_delivery":  nil,
	}
	var destination, err = s.store.NotificationDestinations().Get(r.Context(), serverID, userID)

	if err == nil {
		response["destination"] = destination
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	delivery, err := s.store.NotificationDestinations().LastUserDelivery(r.Context(), serverID, userID)
	if err == nil {
		response["last_delivery"] = lastDelivery{At: delivery.DeliveredAt, Error: delivery.LastError, Attempts: delivery.Attempts}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return reply(w, r, http.StatusOK, response)
}

func (s *Server) handleNotificationDestinationPut(w http.ResponseWriter, r *http.Request) error {
	body, err := decodeJSON[destinationBody](r)
	if err != nil {
		return err
	}
	serverID, userID, err := s.notificationScope(r, body.ServerID, body.UserID)
	if err != nil {
		return err
	}
	var value = store.NotificationDestination{ServerID: serverID, UserID: userID, Kind: body.Kind, Enabled: body.Enabled, Format: "json"}

	switch body.Kind {
	case "ntfy":
		if !validTopic(body.Topic) {
			return BadRequest("topic must be 1-%d characters of letters, digits, - or _", maxTopicLen)
		}
		if s.cfg.Availability.UserNtfyBaseURL == "" {
			return Conflict("per-user ntfy is not configured on this server")
		}
		value.Topic = body.Topic
	case "webhook":
		if principalFrom(r).Role != roleOperator {
			return Forbidden("only an operator can set a webhook destination")
		}
		if err := validateWebhookURL(body.URL); err != nil {
			return err
		}
		if body.Format != "" && body.Format != "json" && body.Format != "ntfy" {
			return BadRequest("format must be json or ntfy")
		}
		value.URL = body.URL
		if body.Format != "" {
			value.Format = body.Format
		}
	default:
		return BadRequest("kind must be ntfy or webhook")
	}
	if err := s.store.NotificationDestinations().Put(r.Context(), value); err != nil {
		return err
	}
	return s.replyDestination(w, r, serverID, userID)
}

func (s *Server) handleNotificationDestinationDelete(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := s.notificationScope(r, "", "")
	if err != nil {
		return err
	}
	if err := s.store.NotificationDestinations().Delete(r.Context(), serverID, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return NotFound("no notification destination is set")
		}
		return err
	}
	return reply(w, r, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleNotificationTest(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := s.notificationScope(r, "", "")
	if err != nil {
		return err
	}
	destination, err := s.store.NotificationDestinations().Get(r.Context(), serverID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return NotFound("no notification destination is set")
	}
	if err != nil {
		return err
	}
	if destination.Kind == "ntfy" && s.cfg.Availability.UserNtfyBaseURL == "" {
		return Conflict("per-user ntfy is not configured on this server")
	}
	if wait := s.notifyTests.take(serverID+"\x00"+userID, s.clock.Now()); wait > 0 {
		return TooManyRequests(CodeRateLimited, "a test notification was sent recently; try again later", wait)
	}
	if err := engine.DeliverToDestination(r.Context(), engine.NewNotificationClient(), s.cfg.Availability.UserNtfyBaseURL, destination, 0, testPayload, testMessage, ""); err != nil {
		return BadGateway(err.Error())
	}
	return reply(w, r, http.StatusOK, map[string]bool{"delivered": true})
}

func validTopic(topic string) bool {
	if topic == "" || len(topic) > maxTopicLen {
		return false
	}
	for _, c := range topic {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// validateWebhookURL applies the same rules as availability.webhook_url.
func validateWebhookURL(raw string) error {
	var parsed, err = url.Parse(raw)

	if err != nil || strings.TrimSpace(raw) != raw || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		return BadRequest("url must be an absolute HTTP(S) URL without user credentials or a fragment")
	}
	return nil
}

// testNotificationLimiter allows one test notification per user per minute. It
// is in-memory and bounded; losing it on restart only forgives one extra test.
type testNotificationLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newTestNotificationLimiter() *testNotificationLimiter {
	return &testNotificationLimiter{last: make(map[string]time.Time)}
}

// take records a test and returns zero, or returns how long the user must wait.
func (l *testNotificationLimiter) take(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if at, ok := l.last[key]; ok && now.Sub(at) < testNotificationGap {
		return testNotificationGap - now.Sub(at)
	}
	if _, tracked := l.last[key]; !tracked && len(l.last) >= testNotificationsCap {
		l.evict(now)
	}
	l.last[key] = now
	return 0
}

// evict drops expired entries, then the oldest one if the map is still full.
func (l *testNotificationLimiter) evict(now time.Time) {
	var oldest string
	var oldestAt time.Time

	for key, at := range l.last {
		if now.Sub(at) >= testNotificationGap {
			delete(l.last, key)
			continue
		}
		if oldest == "" || at.Before(oldestAt) {
			oldest, oldestAt = key, at
		}
	}
	if len(l.last) >= testNotificationsCap {
		delete(l.last, oldest)
	}
}
