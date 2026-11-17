package tiktok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/nyaruka/courier/v26/core/channels"
	"github.com/nyaruka/courier/v26/core/models"
	"github.com/nyaruka/courier/v26/handlers"
	"github.com/nyaruka/gocommon/svclogs"
	"github.com/nyaruka/gocommon/urns"
)

// TikTok accepts JPG and PNG up to 3MB. Images can't be combined with text in one message.
var mediaSupport = map[handlers.MediaType]handlers.MediaTypeSupport{
	handlers.MediaTypeImage: {MaxBytes: maxImageBytes, Types: []string{"image/jpeg", "image/png"}},
}

// Send delivers a message to the conversation the URN was received on. Quick replies are sent as a Q&A button
// card because TikTok has no way to attach buttons to a plain text message.
func (h *handler) Send(ctx context.Context, msg *models.MsgOut, res *channels.SendResult, clog *models.ChannelLog) error {
	ch := msg.Channel()
	token := ch.StringConfigForKey(models.ConfigAuthToken, "")
	businessID := ch.StringConfigForKey(configBusinessID, "")
	if businessID == "" {
		businessID = string(ch.Address())
	}
	if token == "" || businessID == "" {
		return channels.ErrChannelConfig
	}

	conversationID, err := conversationFromURN(msg.URN())
	if err != nil {
		clog.Error(&svclogs.Error{Message: fmt.Sprintf("unable to read conversation from URN: %s", err)})
		return channels.ErrMessageInvalid
	}
	if conversationID == "" {
		clog.Error(&svclogs.Error{Message: "URN has no conversation id"})
		return channels.ErrMessageInvalid
	}

	sent := false

	// a Q&A card carries at most three buttons and needs a title, so anything else falls back to text
	buttons := handlers.FilterSupportedQuickReplies(msg.QuickReplies(), clog, models.QuickReplyTypeText)
	if len(buttons) > 0 && msg.Text() != "" {
		if len(buttons) > 3 {
			clog.Error(&svclogs.Error{Message: "TikTok supports at most 3 quick replies; sending the message as text"})
		} else {
			id, err := h.sendTemplate(ctx, token, businessID, conversationID, msg.Text(), buttons, clog)
			if err != nil {
				return err
			}
			if id != "" {
				res.AddExternalID(id)
			}
			res.AddCost(1)
			sent = true
		}
	}

	if !sent && msg.Text() != "" {
		for _, part := range handlers.SplitMsgByChannel(ch, msg.Text(), maxMsgLength) {
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
			res.AddCost(1)
			sent = true
		}
	}

	attachments, err := handlers.ResolveAttachments(ctx, h.Runtime(), msg.Attachments(), mediaSupport, true, clog)
	if err != nil {
		return err
	}

	for _, att := range attachments {
		if att.Type != handlers.MediaTypeImage {
			clog.Error(&svclogs.Error{Message: fmt.Sprintf("TikTok only supports image attachments, skipping %s", att.ContentType)})
			continue
		}

		data, contentType, err := h.fetchAttachment(ctx, att.URL, att.ContentType, clog)
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
		res.AddCost(1)
		sent = true
	}

	if !sent {
		return channels.ErrMessageInvalid
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

func (h *handler) sendText(ctx context.Context, token, businessID, conversationID, text string, clog *models.ChannelLog) (string, error) {
	return h.callSend(ctx, token, sendRequest{
		BusinessID:    businessID,
		RecipientType: "CONVERSATION",
		Recipient:     conversationID,
		MessageType:   "TEXT",
		Text:          &textPayload{Body: text},
	}, clog)
}

func (h *handler) sendImage(ctx context.Context, token, businessID, conversationID, mediaID string, clog *models.ChannelLog) (string, error) {
	return h.callSend(ctx, token, sendRequest{
		BusinessID:    businessID,
		RecipientType: "CONVERSATION",
		Recipient:     conversationID,
		MessageType:   "IMAGE",
		Image:         &imageBody{MediaID: mediaID},
	}, clog)
}

func (h *handler) sendTemplate(ctx context.Context, token, businessID, conversationID, title string, buttons []models.QuickReply, clog *models.ChannelLog) (string, error) {
	card := &outboundTemplate{Type: "QA_BUTTON_CARD", Title: truncate(title, 40)}

	for i, button := range buttons {
		card.Buttons = append(card.Buttons, outboundButton{
			Type:  "REPLY",
			Title: truncate(button.GetText(), 20),
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

func (h *handler) callSend(ctx context.Context, token string, payload sendRequest, clog *models.ChannelLog) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/business/message/send/", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	return h.callAPI(req, token, clog)
}

// uploadImage posts the bytes to TikTok and returns the media id to send. The id is valid for 30 days.
func (h *handler) uploadImage(ctx context.Context, token, businessID string, data []byte, contentType string, clog *models.ChannelLog) (string, error) {
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
	if err := h.callAPIInto(req, token, &parsed, clog); err != nil {
		return "", err
	}
	if parsed.Data.MediaID == "" {
		return "", channels.ErrResponseUnexpected
	}
	return parsed.Data.MediaID, nil
}

// fetchAttachment downloads an outbound attachment so it can be uploaded to TikTok. TikTok needs a media id,
// not a URL.
func (h *handler) fetchAttachment(ctx context.Context, url, contentType string, clog *models.ChannelLog) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}

	resp, body, err := h.RequestHTTP(req, clog)
	if err != nil {
		return nil, "", channels.ErrConnectionFailed
	}
	if resp == nil || resp.StatusCode/100 != 2 {
		clog.Error(&svclogs.Error{Message: "unable to fetch attachment for upload"})
		return nil, "", channels.ErrResponseStatus
	}
	if len(body) > maxImageBytes {
		clog.Error(&svclogs.Error{Message: fmt.Sprintf("attachment exceeds TikTok's %d byte limit", maxImageBytes)})
		return nil, "", channels.ErrMessageInvalid
	}

	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	if contentType != "image/jpeg" && contentType != "image/png" {
		clog.Error(&svclogs.Error{Message: fmt.Sprintf("TikTok only accepts JPG and PNG images, got %s", contentType)})
		return nil, "", channels.ErrMessageInvalid
	}

	return body, contentType, nil
}

func (h *handler) callAPI(req *http.Request, token string, clog *models.ChannelLog) (string, error) {
	var parsed apiResponse
	if err := h.callAPIInto(req, token, &parsed, clog); err != nil {
		return "", err
	}
	return parsed.Data.Message.MessageID, nil
}

// callAPIInto performs a request and maps TikTok's transport and application errors onto courier's send
// error classes. TikTok answers most failures with HTTP 200 and a non-zero code, so the code is the real
// status.
func (h *handler) callAPIInto(req *http.Request, token string, parsed *apiResponse, clog *models.ChannelLog) error {
	req.Header.Set("Access-Token", token)

	resp, body, err := h.RequestHTTP(req, clog)
	if err != nil {
		return channels.ErrConnectionFailed
	}
	if resp == nil {
		return channels.ErrConnectionFailed
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return channels.ErrConnectionThrottled
	case resp.StatusCode/100 == 5:
		return channels.ErrConnectionFailed
	case resp.StatusCode/100 != 2:
		clog.Error(&svclogs.Error{Message: fmt.Sprintf("TikTok returned HTTP %d", resp.StatusCode)})
		return channels.ErrResponseStatus
	}

	if err := json.Unmarshal(body, parsed); err != nil {
		clog.Error(&svclogs.Error{Message: "unable to parse TikTok response"})
		return channels.ErrResponseUnparseable
	}

	if parsed.Code == 0 {
		return nil
	}

	clog.Error(&svclogs.Error{Message: fmt.Sprintf("TikTok error %d: %s", parsed.Code, parsed.Message)})

	switch parsed.Code {
	case 40100: // rate limited (10 QPS)
		return channels.ErrConnectionThrottled
	case 40001, 40105: // no permission, bad or expired token: needs reauthorization
		return channels.ErrChannelConfig
	case 51065: // system error
		return channels.ErrConnectionFailed
	default:
		return channels.ErrResponseContent
	}
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
