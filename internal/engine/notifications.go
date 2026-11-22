package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/store"
)

const (
	notificationBatch = 10
	// ntfyUnconfiguredDelay keeps events for destinations that cannot be
	// delivered yet from being retried every minute.
	ntfyUnconfiguredDelay = 6 * time.Hour
)

// NewNotificationClient is the HTTP client for every notification delivery: a
// bounded timeout and no redirects, so a receiver cannot bounce a request
// somewhere else.
func NewNotificationClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// NotificationsOnce drains the global outbox (only while the global webhook is
// configured) and, independently, the per-user outbox.
func (e *Engine) NotificationsOnce(ctx context.Context) error {
	var client = NewNotificationClient()

	if e.cfg.Availability.WebhookURL != "" {
		if err := e.globalNotifications(ctx, client); err != nil {
			return err
		}
	}
	return e.userNotifications(ctx, client)
}

func (e *Engine) globalNotifications(ctx context.Context, client *http.Client) error {
	for i := 0; i < notificationBatch; i++ {
		notification, err := e.store.Requests().ClaimNotification(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		message, err := availabilityMessage(notification.Payload, e.cfg.Availability.WebhookFormat)
		if err != nil {
			if err := e.store.Requests().FinishNotification(ctx, notification.ID, "invalid availability event", time.Hour); err != nil {
				return err
			}
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Availability.WebhookURL, strings.NewReader(message.body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", message.contentType)
		if e.cfg.Availability.WebhookFormat == "ntfy" {
			req.Header.Set("Title", message.title)
			req.Header.Set("Tags", "popcorn")
		}
		req.Header.Set("X-Reelay-Event-ID", strconv.FormatInt(notification.ID, 10))
		failure := ""
		if err := deliverWebhook(client, req); err != nil {
			failure = err.Error()
		}
		delay := min(time.Minute*time.Duration(1<<min(notification.Attempts, 10)), 24*time.Hour)
		if err := e.store.Requests().FinishNotification(ctx, notification.ID, failure, delay); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) userNotifications(ctx context.Context, client *http.Client) error {
	for i := 0; i < notificationBatch; i++ {
		var notification, err = e.store.Requests().ClaimUserNotification(ctx)

		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !notification.HasDestination {
			if err := e.store.Requests().DropUserNotification(ctx, notification.ID, "destination removed"); err != nil {
				return err
			}
			continue
		}
		message, err := availabilityMessage(notification.Payload, "ntfy")
		if err != nil {
			if err := e.store.Requests().FinishUserNotification(ctx, notification.ID, "invalid availability event", time.Hour); err != nil {
				return err
			}
			continue
		}
		var delay = min(time.Minute*time.Duration(1<<min(notification.Attempts, 10)), 24*time.Hour)
		var failure = ""

		if notification.Destination.Kind == "ntfy" && e.cfg.Availability.UserNtfyBaseURL == "" {
			failure, delay = "per-user ntfy is not configured", ntfyUnconfiguredDelay
		} else if err := deliverToDestination(ctx, client, e.cfg.Availability.UserNtfyBaseURL, notification.Destination, notification.ID, notification.Payload, message, e.playableURL(notification.Payload)); err != nil {
			failure = err.Error()
		}
		if err := e.store.Requests().FinishUserNotification(ctx, notification.ID, failure, delay); err != nil {
			return err
		}
	}
	return nil
}

// playableURL returns the Jellyfin link for the event's item, or "" when the
// event's server has no configured base URL.
func (e *Engine) playableURL(payload string) string {
	var event struct {
		ServerID string `json:"server_id"`
		ItemID   string `json:"jellyfin_item_id"`
	}

	if json.Unmarshal([]byte(payload), &event) != nil || event.ItemID == "" {
		return ""
	}
	var base = e.cfg.Availability.JellyfinServers[event.ServerID]

	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/web/index.html#!/details?id=" + url.QueryEscape(event.ItemID)
}

// DeliverToDestination posts one notification to a user's destination. payload
// is the JSON event, message the readable text, click an optional link. The
// per-user ntfy server is availability.user_ntfy_base_url.
func DeliverToDestination(ctx context.Context, client *http.Client, ntfyBaseURL string, destination store.NotificationDestination, eventID int64, payload, message, click string) error {
	return deliverToDestination(ctx, client, ntfyBaseURL, destination, eventID, payload, notificationMessage{title: "Reelay: ready to watch", body: message, contentType: "text/plain; charset=utf-8"}, click)
}

func deliverToDestination(ctx context.Context, client *http.Client, ntfyBaseURL string, destination store.NotificationDestination, eventID int64, payload string, message notificationMessage, click string) error {
	var target, body, contentType = destination.URL, payload, "application/json"
	var ntfy = destination.Kind == "ntfy" || destination.Format == "ntfy"

	if destination.Kind == "ntfy" {
		var err error

		target, err = url.JoinPath(ntfyBaseURL, destination.Topic)
		if err != nil {
			return errors.New("per-user ntfy base URL is invalid")
		}
	}
	if ntfy {
		body, contentType = message.body, message.contentType
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		return errors.New("notification destination URL is invalid")
	}
	req.Header.Set("Content-Type", contentType)
	if ntfy {
		req.Header.Set("Title", message.title)
		req.Header.Set("Tags", "popcorn")
		if click != "" {
			req.Header.Set("Click", click)
		}
	}
	req.Header.Set("X-Reelay-Event-ID", strconv.FormatInt(eventID, 10))
	return send(client, req, "notification destination could not be reached")
}

func deliverWebhook(client *http.Client, req *http.Request) error {
	return send(client, req, "availability webhook could not be reached")
}

func send(client *http.Client, req *http.Request, unreachable string) error {
	response, err := client.Do(req)
	if err != nil {
		return errors.New(unreachable)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

type notificationMessage struct {
	title       string
	body        string
	contentType string
}

func availabilityMessage(payload, format string) (notificationMessage, error) {
	var event struct {
		Title string `json:"title"`
	}

	if format != "ntfy" {
		return notificationMessage{body: payload, contentType: "application/json"}, nil
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return notificationMessage{}, err
	}
	if strings.TrimSpace(event.Title) == "" {
		return notificationMessage{}, errors.New("availability event has no title")
	}
	var message = notificationMessage{
		title:       "Reelay: ready to watch",
		body:        event.Title + " is available in Jellyfin.",
		contentType: "text/plain; charset=utf-8",
	}
	return message, nil
}
