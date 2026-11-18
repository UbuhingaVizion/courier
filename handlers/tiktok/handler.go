// Package tiktok implements the TikTok Business Messaging channel.
//
// API docs: https://business-api.tiktok.com/portal/docs/business-messaging/v1.3
// Webhook signature: https://developers.tiktok.com/docs/en/webhooks-verification
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

	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
)

const (
	// apiBaseURL pins the Business Messaging API version in one place.
	apiBaseURL = "https://business-api.tiktok.com/open_api/v1.3"

	maxMsgLength  = 6000
	maxBodyBytes  = int64(1024 * 1024)
	maxImageBytes = 3 * 1024 * 1024
	maxMediaBytes = 25 * 1024 * 1024

	signatureHeader = "TikTok-Signature"
	signatureMaxAge = 5 * time.Minute

	configBusinessID   = "business_id"
	configClientSecret = "client_secret"
)

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(
		courier.ChannelType("TT"),
		"TikTok",
		handlers.WithRedactConfigKeys(courier.ConfigAuthToken, configClientSecret, "refresh_token", "webhook_secret"),
	)}
}

func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "receive", courier.ChannelLogTypeMsgReceive, h.receive)
	return nil
}

// receive verifies the delivery signature over the raw body before parsing it, and answers a bad signature with
// 403 rather than the generic request error.
func (h *handler) receive(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request, clog *courier.ChannelLog) ([]courier.Event, error) {
	body, err := handlers.ReadBody(r, maxBodyBytes)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	if err := h.verifySignature(channel, r.Header.Get(signatureHeader), body); err != nil {
		return nil, courier.WriteError(w, http.StatusForbidden, err)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	msgs, ignored, err := h.parseEvent(ctx, channel, &env, clog)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}
	if ignored != "" {
		return nil, handlers.WriteAndLogRequestIgnored(ctx, h, channel, w, r, ignored)
	}

	return handlers.WriteMsgsAndResponse(ctx, h, msgs, w, r, clog)
}

// verifySignature follows the documented scheme: s = HMAC-SHA256(signing secret, t + "." + raw body), hex encoded.
func (h *handler) verifySignature(channel courier.Channel, header string, body []byte) error {
	if header == "" {
		return errors.New("missing TikTok-Signature header")
	}

	// TikTok signs with the app secret. A deployment can keep a separate signing secret.
	secret := channel.StringConfigForKey("webhook_secret", "")
	if secret == "" {
		secret = channel.StringConfigForKey(configClientSecret, "")
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
