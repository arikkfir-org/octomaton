// Package webhook receives GitHub App webhooks: it verifies signatures, drops
// duplicate deliveries and hands accepted events to a bounded worker pool.
package webhook

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/go-github/v92/github"
	"octomaton.dev/internal/metrics"
)

// MaxBodyBytes is the largest payload accepted (GitHub caps payloads at 25 MB).
const MaxBodyBytes = 25 << 20

// Router turns a parsed webhook payload into a job. A job with a nil Run means
// the event is ignored, for the returned reason.
type Router interface {
	Route(event, deliveryID string, payload any) (job Job, reason string)
}

// handledEvents are the events parsed and routed; others are acknowledged and ignored.
var handledEvents = map[string]bool{
	"push":          true,
	"pull_request":  true,
	"merge_group":   true,
	"check_run":     true,
	"check_suite":   true,
	"issue_comment": true,
}

// relayedEvents are forwarded to the Forwarder after signature verification.
var relayedEvents = map[string]bool{"push": true, "pull_request": true}

// Forwarder receives verified push and pull_request deliveries (*relay.Relay implements it).
type Forwarder interface {
	Forward(header http.Header, body []byte)
}

// Handler serves POST /github/hooks.
type Handler struct {
	Secret  []byte
	Router  Router
	Pool    *Pool
	Dedupe  *Dedupe
	Metrics *metrics.Metrics
	Logger  *slog.Logger
	// Relay, when set, receives verified push and pull_request deliveries.
	Relay Forwarder
}

func metricEvent(event string) string {
	if handledEvents[event] || event == "ping" {
		return event
	}
	return "other"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	label := metricEvent(event)
	h.Metrics.WebhooksReceived.WithLabelValues(label).Inc()
	log := h.Logger.With("event", event, "delivery", delivery)

	reject := func(status int, reason, message string) {
		h.Metrics.WebhooksRejected.WithLabelValues(label, reason).Inc()
		log.Warn("Rejected webhook", "status", status, "reason", reason, "detail", message)
		http.Error(w, message, status)
	}
	respond := func(status int, message string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = fmt.Fprintln(w, message)
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		reject(http.StatusMethodNotAllowed, "method", "only POST is supported")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			reject(http.StatusRequestEntityTooLarge, "body", "payload too large")
		} else {
			reject(http.StatusBadRequest, "body", "could not read payload")
		}
		return
	}
	if !VerifySignature(h.Secret, body, r.Header.Get(SignatureHeader)) {
		reject(http.StatusUnauthorized, "signature", "invalid signature")
		return
	}
	switch {
	case event == "":
		reject(http.StatusBadRequest, "event", "missing X-GitHub-Event header")
		return
	case event == "ping":
		respond(http.StatusOK, "pong")
		return
	}
	if h.Relay != nil && relayedEvents[event] {
		h.Relay.Forward(r.Header, body)
	}
	if !handledEvents[event] {
		log.Debug("Ignoring unhandled event")
		respond(http.StatusAccepted, "ignored: unhandled event")
		return
	}

	payload, err := github.ParseWebHook(event, body)
	if err != nil {
		reject(http.StatusBadRequest, "payload", "invalid payload")
		return
	}
	job, reason := h.Router.Route(event, delivery, payload)
	if job.Run == nil {
		log.Debug("Ignoring event", "reason", reason)
		respond(http.StatusAccepted, "ignored: "+reason)
		return
	}
	if delivery != "" && !h.Dedupe.Add(delivery) {
		log.Info("Ignoring duplicate delivery")
		respond(http.StatusAccepted, "ignored: duplicate delivery")
		return
	}
	if !h.Pool.Submit(job) {
		if delivery != "" {
			h.Dedupe.Remove(delivery)
		}
		reject(http.StatusServiceUnavailable, "queue_full", "busy, please redeliver later")
		return
	}
	log.Info("Accepted webhook", "job", job.Name)
	respond(http.StatusAccepted, "accepted")
}
