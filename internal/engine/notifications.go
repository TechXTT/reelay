package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/store"
)

func (e *Engine) NotificationsOnce(ctx context.Context) error {
	if e.cfg.Availability.WebhookURL == "" {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i := 0; i < 10; i++ {
		notification, err := e.store.Requests().ClaimNotification(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		payload, contentType, err := availabilityMessage(notification.Payload, e.cfg.Availability.WebhookFormat)
		if err != nil {
			if err := e.store.Requests().FinishNotification(ctx, notification.ID, "invalid availability event", time.Hour); err != nil {
				return err
			}
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Availability.WebhookURL, strings.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentType)
		if e.cfg.Availability.WebhookFormat == "ntfy" {
			req.Header.Set("Title", "Reelay: ready to watch")
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

func deliverWebhook(client *http.Client, req *http.Request) error {
	response, err := client.Do(req)
	if err != nil {
		return errors.New("availability webhook could not be reached")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

func availabilityMessage(payload, format string) (string, string, error) {
	var event struct {
		Title string `json:"title"`
	}

	if format != "ntfy" {
		return payload, "application/json", nil
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(event.Title) == "" {
		return "", "", errors.New("availability event has no title")
	}
	return event.Title + " is available in Jellyfin.", "text/plain; charset=utf-8", nil
}
