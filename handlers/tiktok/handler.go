// Package tiktok implements the TikTok Business Messaging channel.
//
// API docs: https://business-api.tiktok.com/portal/docs/business-messaging/v1.3
// Webhook signature: https://developers.tiktok.com/docs/en/webhooks-verification
//
// TikTok signs every delivery with an HMAC over the raw request body and the
// timestamp in the TikTok-Signature header. The signature is checked before the
// body is decoded, so the raw bytes are read here rather than by the route's
// JSON decoder.
package tiktok

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nyaruka/courier/v26/core/channels"
	"github.com/nyaruka/courier/v26/core/models"
	"github.com/nyaruka/courier/v26/handlers"
	"github.com/nyaruka/courier/v26/runtime"
)

const (
	// apiBaseURL pins the Business Messaging API version in one place.
	apiBaseURL = "https://business-api.tiktok.com/open_api/v1.3"

	maxMsgLength  = 6000 // TikTok's limit for a text message body
	maxBodyBytes  = int64(1024 * 1024)
	maxImageBytes = 3 * 1024 * 1024 // TikTok only accepts JPG/PNG up to 3MB
	maxMediaBytes = 25 * 1024 * 1024

	signatureHeader = "TikTok-Signature"
	signatureMaxAge = 5 * time.Minute

	configBusinessID   = "business_id"
	configClientSecret = "client_secret"
	configClientKey    = "client_key"
)

func init() {
	channels.RegisterHandler(newHandler)
}

type handler struct {
	handlers.BaseHandler
}

func newHandler(rt *runtime.Runtime, r *channels.Routes) channels.Handler {
	h := &handler{handlers.NewBaseHandler(rt, models.ChannelType("TT"), "TikTok",
		handlers.WithRedactConfigKeys(models.ConfigAuthToken, configClientSecret, "refresh_token", "webhook_secret"))}
	r.AddReceive(h, http.MethodPost, "receive", channels.ReceiveKindMsg, h.receive)
	return h
}

// signatureError marks a request that failed webhook verification so the handler can answer 403 rather than
// the generic 400.
type signatureError struct{ err error }

func (e *signatureError) Error() string { return e.err.Error() }
func (e *signatureError) Unwrap() error { return e.err }

func (h *handler) RespondError(ctx context.Context, w http.ResponseWriter, err error) error {
	var sigErr *signatureError
	if errors.As(err, &sigErr) {
		return channels.RespondError(w, http.StatusForbidden, sigErr)
	}
	return channels.RespondError(w, http.StatusBadRequest, err)
}

// receive verifies the delivery signature, then maps the event onto courier's incoming model.
func (h *handler) receive(ctx context.Context, ch *models.Channel, r *http.Request, in *channels.Received, clog *models.ChannelLog) error {
	body, err := handlers.ReadBody(r, maxBodyBytes)
	if err != nil {
		return fmt.Errorf("unable to read request body: %w", err)
	}

	if err := h.verifySignature(ch, r.Header.Get(signatureHeader), body); err != nil {
		return &signatureError{err}
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("unable to parse request JSON: %w", err)
	}

	return h.handleEvent(ctx, ch, &env, in, clog)
}

// verifySignature follows the documented scheme: s = HMAC-SHA256(client_secret, t + "." + raw body), hex encoded.
// The timestamp is part of the signed payload and is also checked for age so a captured delivery can't be
// replayed later.
func (h *handler) verifySignature(ch *models.Channel, header string, body []byte) error {
	if header == "" {
		return errors.New("missing TikTok-Signature header")
	}

	// TikTok signs with the app secret. A deployment can keep a separate signing secret, in which case the
	// channel stores it as webhook_secret and that wins.
	secret := ch.StringConfigForKey("webhook_secret", "")
	if secret == "" {
		secret = ch.StringConfigForKey(configClientSecret, "")
	}
	if secret == "" {
		return errors.New("channel is missing its signing secret")
	}

	timestamp, signature, err := parseSignature(header)
	if err != nil {
		return err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return errors.New("invalid request signature")
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid signature timestamp: %w", err)
	}
	age := time.Since(time.Unix(seconds, 0))
	if age > signatureMaxAge || age < -signatureMaxAge {
		return fmt.Errorf("signature timestamp outside the allowed window: %s", age)
	}

	return nil
}

// parseSignature splits t=<unix seconds>,s=<hex signature> without assuming an order.
func parseSignature(header string) (string, string, error) {
	var timestamp, signature string

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			timestamp = value
		case "s":
			signature = value
		}
	}

	if timestamp == "" || signature == "" {
		return "", "", errors.New("malformed TikTok-Signature header")
	}
	return timestamp, signature, nil
}
