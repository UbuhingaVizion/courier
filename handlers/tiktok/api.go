package tiktok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/gocommon/urns"
)

// TikTok accepts JPG and PNG up to 3MB. Images can't be combined with text in one message.
var mediaSupport = map[handlers.MediaType]handlers.MediaTypeSupport{
	handlers.MediaTypeImage: {MaxBytes: maxImageBytes, Types: []string{"image/jpeg", "image/png"}},
}

// Send delivers a message to the conversation the URN was received on. Quick replies are sent as a Q&A button
// card because TikTok has no way to attach buttons to a plain text message.
func (h *handler) Send(ctx context.Context, msg courier.MsgOut, res *courier.SendResult, clog *courier.ChannelLog) error {
	channel := msg.Channel()
	token := channel.StringConfigForKey(courier.ConfigAuthToken, "")
	businessID := channel.StringConfigForKey(configBusinessID, "")
	if businessID == "" {
		businessID = channel.Address()
	}
	if token == "" || businessID == "" {
		return courier.ErrChannelConfig
	}

	conversationID, err := conversationFromURN(msg.URN())
	if err != nil || conversationID == "" {
		return courier.ErrFailedWithReason("invalid_urn", "URN has no TikTok conversation")
	}

	sent := false

	// a Q&A card carries at most three buttons and needs a title, so anything else falls back to text
	buttons := msg.QuickReplies()
	if len(buttons) > 0 && msg.Text() != "" {
		if len(buttons) > 3 {
			clog.Error(courier.NewChannelError("unsupported_quick_replies", "", "TikTok supports at most 3 quick replies; sending text"))
		} else {
			id, err := h.sendTemplate(ctx, token, businessID, conversationID, msg.Text(), buttons, clog)
			if err != nil {
				return err
			}
			if id != "" {
				res.AddExternalID(id)
			}
			sent = true
		}
	}

	if !sent && msg.Text() != "" {
		for _, part := range handlers.SplitMsgByChannel(channel, msg.Text(), maxMsgLength) {
			if strings.TrimSpace(part) == "" {
				continue
			}
			id, err := h.sendText(ctx, token, businessID, conversationID, part, clog)
			if err != nil {
				return err
			}
			if id != "" {
				res.AddExternalID(id)
			}
			sent = true
		}
	}

	attachments, err := handlers.ResolveAttachments(ctx, h.Backend(), msg.Attachments(), mediaSupport, true, clog)
	if err != nil {
		return err
	}

	for _, attachment := range attachments {
		if attachment.Type != handlers.MediaTypeImage {
			clog.Error(courier.NewChannelError("unsupported_attachment", "", fmt.Sprintf("TikTok only supports images, skipping %s", attachment.ContentType)))
			continue
		}

		data, contentType, err := h.fetchAttachment(ctx, attachment.URL, attachment.ContentType, clog)
		if err != nil {
			return err
		}

		mediaID, err := h.uploadImage(ctx, token, businessID, data, contentType, clog)
		if err != nil {
			return err
		}

		id, err := h.sendImage(ctx, token, businessID, conversationID, mediaID, clog)
		if err != nil {
			return err
		}
		if id != "" {
			res.AddExternalID(id)
		}
		sent = true
	}

	if !sent {
		return courier.ErrFailedWithReason("empty_message", "message has nothing TikTok can send")
	}
	return nil
}

func conversationFromURN(urn urns.URN) (string, error) {
	query, err := urn.Query()
	if err != nil {
		return "", err
	}
	token := query.Get("conversation")
	if token == "" {
		return "", nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(token); err == nil {
		return string(decoded), nil
	}
	return token, nil
}

func (h *handler) sendText(ctx context.Context, token, businessID, conversationID, text string, clog *courier.ChannelLog) (string, error) {
	return h.callSend(ctx, token, sendRequest{
		BusinessID:    businessID,
		RecipientType: "CONVERSATION",
		Recipient:     conversationID,
		MessageType:   "TEXT",
		Text:          &textPayload{Body: text},
	}, clog)
}

func (h *handler) sendImage(ctx context.Context, token, businessID, conversationID, mediaID string, clog *courier.ChannelLog) (string, error) {
	return h.callSend(ctx, token, sendRequest{
		BusinessID:    businessID,
		RecipientType: "CONVERSATION",
		Recipient:     conversationID,
		MessageType:   "IMAGE",
		Image:         &imageBody{MediaID: mediaID},
	}, clog)
}

func (h *handler) sendTemplate(ctx context.Context, token, businessID, conversationID, title string, buttons []string, clog *courier.ChannelLog) (string, error) {
	card := &outboundTemplate{Type: "QA_BUTTON_CARD", Title: truncate(title, 40)}

	for i, button := range buttons {
		card.Buttons = append(card.Buttons, outboundButton{
			Type:  "REPLY",
			Title: truncate(button, 20),
			ID:    fmt.Sprintf("reply_%d", i+1),
		})
	}

	return h.callSend(ctx, token, sendRequest{
		BusinessID:    businessID,
		RecipientType: "CONVERSATION",
		Recipient:     conversationID,
		MessageType:   "TEMPLATE",
		Template:      card,
	}, clog)
}

func (h *handler) callSend(ctx context.Context, token string, payload sendRequest, clog *courier.ChannelLog) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/business/message/send/", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	var parsed apiResponse
	if err := h.callAPI(req, token, &parsed, clog); err != nil {
		return "", err
	}
	return parsed.Data.Message.MessageID, nil
}

// uploadImage posts the bytes to TikTok and returns the media id to send. The id is valid for 30 days.
func (h *handler) uploadImage(ctx context.Context, token, businessID string, data []byte, contentType string, clog *courier.ChannelLog) (string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	if err := writer.WriteField("business_id", businessID); err != nil {
		return "", err
	}
	if err := writer.WriteField("media_type", "IMAGE"); err != nil {
		return "", err
	}

	filename := "image.jpg"
	if contentType == "image/png" {
		filename = "image.png"
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/business/message/media/upload/", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	var parsed apiResponse
	if err := h.callAPI(req, token, &parsed, clog); err != nil {
		return "", err
	}
	if parsed.Data.MediaID == "" {
		return "", courier.ErrResponseUnexpected
	}
	return parsed.Data.MediaID, nil
}

// fetchAttachment downloads an outbound attachment so it can be uploaded to TikTok.
func (h *handler) fetchAttachment(ctx context.Context, url, contentType string, clog *courier.ChannelLog) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}

	resp, body, err := h.RequestHTTP(req, clog)
	if err != nil || resp == nil || resp.StatusCode/100 != 2 {
		clog.Error(courier.NewChannelError("attachment_fetch", "", "unable to fetch attachment for upload"))
		return nil, "", courier.ErrResponseStatus
	}
	if len(body) > maxImageBytes {
		clog.Error(courier.NewChannelError("attachment_too_large", "", fmt.Sprintf("attachment exceeds %d bytes", maxImageBytes)))
		return nil, "", courier.ErrFailedWithReason("attachment_too_large", "attachment exceeds TikTok's limit")
	}

	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	if contentType != "image/jpeg" && contentType != "image/png" {
		return nil, "", courier.ErrFailedWithReason("unsupported_attachment", "TikTok only accepts JPG and PNG")
	}

	return body, contentType, nil
}

// callAPI performs a request and maps TikTok's transport and application errors onto courier's send error
// classes. TikTok answers most failures with HTTP 200 and a non-zero code, so the code is the real status.
func (h *handler) callAPI(req *http.Request, token string, parsed *apiResponse, clog *courier.ChannelLog) error {
	req.Header.Set("Access-Token", token)

	resp, body, err := h.RequestHTTP(req, clog)
	if err != nil || resp == nil {
		return courier.ErrConnectionFailed
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return courier.ErrConnectionThrottled
	case resp.StatusCode/100 == 5:
		return courier.ErrConnectionFailed
	case resp.StatusCode/100 != 2:
		clog.Error(courier.NewChannelError("response_status", "", fmt.Sprintf("TikTok returned HTTP %d", resp.StatusCode)))
		return courier.ErrResponseStatus
	}

	if err := json.Unmarshal(body, parsed); err != nil {
		clog.Error(courier.NewChannelError("response_unparseable", "", "unable to parse TikTok response"))
		return courier.ErrResponseUnparseable
	}

	if parsed.Code == 0 {
		return nil
	}

	clog.Error(courier.NewChannelError("tiktok_error", strconv.Itoa(parsed.Code), parsed.Message))

	switch parsed.Code {
	case 40100:
		return courier.ErrConnectionThrottled
	case 40001, 40105:
		return courier.ErrChannelConfig
	case 51065:
		return courier.ErrConnectionFailed
	default:
		return courier.ErrFailedWithReason(strconv.Itoa(parsed.Code), parsed.Message)
	}
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
