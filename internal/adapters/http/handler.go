package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"octomaton.dev/internal/services/ci"
	"octomaton.dev/internal/system/metrics"
)

// MaxBodyBytes is the largest payload accepted (GitHub caps payloads at 25 MB).
const MaxBodyBytes = 25 << 20

// Decoder turns verified deliveries into events; the GitHub adapter's App implements it.
type Decoder interface {
	// Handles reports whether Decode handles an event, by its X-GitHub-Event name.
	Handles(event string) bool
	// Decode turns a delivery into an event. It returns no event, and the reason, for deliveries
	// Octomaton ignores, and an error for payloads it cannot parse.
	Decode(event, delivery string, body []byte) (ci.Event, string, error)
}

// EventHandler does what an event asks for; the runs service implements it.
type EventHandler interface {
	Handle(ctx context.Context, ev ci.Event)
}

// relayedEvents are forwarded to the Forwarder after signature verification. Push receivers such as Argo CD's
// webhook endpoint accept pushes and pings and answer every other event with an error.
var relayedEvents = map[string]bool{"push": true, "ping": true}

// Forwarder receives verified push and ping deliveries (*relay.Relay implements it).
type Forwarder interface {
	Forward(header http.Header, body []byte)
}

// Handler serves POST /github/hooks: it verifies signatures, decodes deliveries, drops duplicates
// and hands the events to a bounded worker pool.
type Handler struct {
	Secret  []byte
	Decoder Decoder
	Events  EventHandler
	Pool    *Pool
	Dedupe  *Dedupe
	Metrics *metrics.Metrics
	Logger  *slog.Logger
	// Relay, when set, receives verified push and ping deliveries.
	Relay Forwarder
}

func (h *Handler) metricEvent(event string) string {
	if event == "ping" || h.Decoder.Handles(event) {
		return event
	}
	return "other"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	label := h.metricEvent(event)
	h.Metrics.WebhookReceived(r.Context(), label)
	log := h.Logger.With("event", event, "delivery", delivery)

	reject := func(status int, reason, message string) {
		h.Metrics.WebhookRejected(r.Context(), label, reason)
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
	if event == "" {
		reject(http.StatusBadRequest, "event", "missing X-GitHub-Event header")
		return
	}
	if h.Relay != nil && relayedEvents[event] {
		h.Relay.Forward(r.Header, body)
	}
	if event == "ping" {
		respond(http.StatusOK, "pong")
		return
	}
	if !h.Decoder.Handles(event) {
		log.Debug("Ignoring unhandled event")
		respond(http.StatusAccepted, "ignored: unhandled event")
		return
	}

	ev, reason, err := h.Decoder.Decode(event, delivery, body)
	if err != nil {
		reject(http.StatusBadRequest, "payload", "invalid payload")
		return
	}
	if ev == nil {
		log.Debug("Ignoring event", "reason", reason)
		respond(http.StatusAccepted, "ignored: "+reason)
		return
	}
	if delivery != "" && !h.Dedupe.Add(delivery) {
		log.Info("Ignoring duplicate delivery")
		respond(http.StatusAccepted, "ignored: duplicate delivery")
		return
	}
	job := Job{Name: ev.String(), Run: func(ctx context.Context) { h.Events.Handle(ctx, ev) }}
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
