package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/netguard"
)

// notify fans out to every enabled rule subscribed to event.
//
// Delivery is best-effort and asynchronous: a slow chat webhook must never
// stall a benchmark run. Supported channels: email (logged for the operator
// MTA), discord and slack — the chat channels via HTTP POST with a JSON
// payload shaped for their incoming webhooks.
func (e *Engine) notify(ctx context.Context, tenantID, event string, fields map[string]any) {
	if e.repos == nil || e.repos.BenchmarkNotifications == nil {
		return
	}
	rules, err := e.repos.BenchmarkNotifications.List(ctx, tenantID)
	if err != nil {
		return
	}
	payload := map[string]any{"event": event, "tenant_id": tenantID, "fields": fields, "at": time.Now().UTC()}
	for _, rule := range rules {
		if !rule.Enabled || !subscribed(rule.Events, event) {
			continue
		}
		go deliverNotification(rule, payload, e.logger)
	}
	_ = domain.BenchmarkNotifyEmail
}

func subscribed(events []string, event string) bool {
	for _, ev := range events {
		if ev == event || ev == "*" || ev == "all" {
			return true
		}
	}
	return false
}

func deliverNotification(rule domain.BenchmarkNotification, payload map[string]any, logger interface {
	Warn(msg string, args ...any)
}) {
	var url, body string
	switch rule.Channel {
	case domain.BenchmarkNotifyDiscord, domain.BenchmarkNotifySlack:
		url = rule.Target
		raw, _ := json.Marshal(payload)
		body = string(raw)
	case domain.BenchmarkNotifyEmail:
		// Email delivery is operator-configured (MTA/relay); record the intent
		// in the log so it is auditable without coupling to an SMTP library.
		logger.Warn("benchmark email notification (no MTA configured; logged only)",
			"to", rule.Target, "event", payload["event"])
		return
	default:
		return
	}
	if url == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Chat payloads use each service's incoming-webhook shape: Discord reads
	// `content`, Slack reads `text`. Sending the raw event JSON would be
	// silently ignored by both.
	var text string
	switch rule.Channel {
	case domain.BenchmarkNotifyDiscord:
		if raw, err := json.Marshal(map[string]string{"content": fmt.Sprintf("Synapass benchmark: %v", payload["event"])}); err == nil {
			text = string(raw)
		}
	case domain.BenchmarkNotifySlack:
		if raw, err := json.Marshal(map[string]string{"text": fmt.Sprintf("Synapass benchmark: %v", payload["event"])}); err == nil {
			text = string(raw)
		}
	default:
		text = body
	}
	// Re-validate at delivery time as well as at write time: the rule may have
	// been stored before this validation existed, and the host may resolve
	// differently now. The client repeats the check at connect time, which is
	// what actually closes the DNS-rebinding gap.
	if _, err := netguard.ValidatePublicURL(ctx, url); err != nil {
		logger.Warn("benchmark notification target refused",
			"channel", string(rule.Channel), "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(text))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := netguard.NewPublicClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("benchmark notification delivery failed", "channel", string(rule.Channel), "error", err)
		return
	}
	defer resp.Body.Close()
}
