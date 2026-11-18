package tiktok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nyaruka/courier"
	"github.com/nyaruka/gocommon/urns"
)

// parseEvent turns one TikTok webhook event into the messages courier writes. Events we understand but don't
// act on are reported as ignored so the response shows what arrived.
func (h *handler) parseEvent(ctx context.Context, channel courier.Channel, env *envelope, clog *courier.ChannelLog) ([]courier.MsgIn, string, error) {
	switch env.Event {
	case "im_receive_msg":
		// handled below
	case "im_receive_msg_eu":
		return nil, "im_receive_msg_eu has no sender or conversation", nil
	case "im_send_msg":
		return nil, "echo of a message sent by the business", nil
	default:
		return nil, fmt.Sprintf("unsupported event: %s", env.Event), nil
	}

	if env.Content == "" {
		return nil, "event has no content", nil
	}

	var c messageContent
	if err := json.Unmarshal([]byte(env.Content), &c); err != nil {
		return nil, "", fmt.Errorf("unable to parse event content: %s", err)
	}

	// the sender is the personal account in the conversation; user_openid is the business account
	if c.FromUser.Role != "personal_account" || c.FromUser.ID == "" {
		return nil, "event was not sent by a personal account", nil
	}
	if c.ConversationID == "" {
		return nil, "event has no conversation id", nil
	}

	urn, err := conversationURN(c.FromUser.ID, c.ConversationID)
	if err != nil {
		return nil, "", fmt.Errorf("unable to build URN: %s", err)
	}

	receivedOn := time.Now()
	if c.Timestamp > 0 {
		receivedOn = time.UnixMilli(c.Timestamp)
	} else if env.CreateTime > 0 {
		receivedOn = time.Unix(env.CreateTime, 0)
	}

	text := ""
	attachment := ""

	switch c.Type {
	case "text":
		if c.Text != nil {
			text = c.Text.Body
		}

	case "share_post":
		if c.SharePost != nil {
			text = c.SharePost.EmbedURL
			if text == "" {
				text = c.SharePost.VideoID
			}
		}

	case "image", "video":
		mediaType := "IMAGE"
		if c.Type == "video" {
			mediaType = "VIDEO"
		}
		data, contentType, err := h.downloadMedia(ctx, channel, &c, mediaType, clog)
		if err != nil {
			clog.Error(courier.NewChannelError("media_download", "", fmt.Sprintf("unable to download %s: %s", c.Type, err)))
			return nil, fmt.Sprintf("unable to download %s", c.Type), nil
		}
		url, err := h.Backend().SaveAttachment(ctx, channel, contentType, data, extensionFor(contentType))
		if err != nil {
			clog.Error(courier.NewChannelError("attachment_save", "", fmt.Sprintf("unable to save attachment: %s", err)))
			return nil, "unable to save attachment", nil
		}
		attachment = url

	case "sticker", "emoji":
		// public URLs needing no access token
		if c.Sticker != nil && c.Sticker.URL != "" {
			attachment = c.Sticker.URL
		} else if c.Emoji != nil && c.Emoji.URL != "" {
			attachment = c.Emoji.URL
		}

	default:
		return nil, fmt.Sprintf("unsupported message type: %s", c.Type), nil
	}

	if text == "" && attachment == "" {
		return nil, fmt.Sprintf("nothing to handle in %s message", c.Type), nil
	}

	msg := h.Backend().NewIncomingMsg(channel, urn, text, c.MessageID, clog).WithReceivedOn(receivedOn.UTC())
	if attachment != "" {
		msg.WithAttachment(attachment)
	}

	return []courier.MsgIn{msg}, "", nil
}

// conversationURN keeps the open_id as the URN path and carries the conversation id in the query so replies
// can address the conversation without changing the contact's identity.
func conversationURN(openID, conversationID string) (urns.URN, error) {
	token := base64.RawURLEncoding.EncodeToString([]byte(conversationID))
	return urns.NewURNFromParts("ext", "tt_"+openID, "conversation="+token, "")
}

// downloadMedia exchanges the message's media id for a temporary URL and fetches the bytes. The URL requires
// the access token in the x-user header, so the bytes are stored before the message is written.
func (h *handler) downloadMedia(ctx context.Context, channel courier.Channel, c *messageContent, mediaType string, clog *courier.ChannelLog) ([]byte, string, error) {
	token := channel.StringConfigForKey(courier.ConfigAuthToken, "")
	if token == "" {
		return nil, "", fmt.Errorf("channel is missing its access token")
	}

	businessID := channel.StringConfigForKey(configBusinessID, "")
	if businessID == "" {
		businessID = channel.Address()
	}
	if businessID == "" {
		return nil, "", fmt.Errorf("channel is missing its business id")
	}

	mediaID := ""
	switch mediaType {
	case "IMAGE":
		if c.Image != nil {
			mediaID = c.Image.MediaID
		}
	case "VIDEO":
		if c.Video != nil {
			mediaID = c.Video.MediaID
		}
	}
	if mediaID == "" {
		return nil, "", fmt.Errorf("event has no media id")
	}

	payload, err := json.Marshal(downloadRequest{
		BusinessID:     businessID,
		ConversationID: c.ConversationID,
		MessageID:      c.MessageID,
		MediaID:        mediaID,
		MediaType:      mediaType,
	})
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/business/message/media/download/", bytes.NewReader(payload))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Token", token)

	resp, body, err := h.RequestHTTP(req, clog)
	if err != nil || resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("download lookup failed")
	}

	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", fmt.Errorf("unable to parse download response: %s", err)
	}
	if parsed.Code != 0 || parsed.Data.DownloadURL == "" {
		return nil, "", fmt.Errorf("download lookup failed with code %d: %s", parsed.Code, parsed.Message)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, parsed.Data.DownloadURL, nil)
	if err != nil {
		return nil, "", err
	}
	// TikTok requires the access token here as well, under a different header than the API calls
	req.Header.Set("x-user", token)

	resp, body, err = h.RequestHTTP(req, clog)
	if err != nil || resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("media download failed")
	}
	if len(body) > maxMediaBytes {
		return nil, "", fmt.Errorf("media exceeds %d bytes", maxMediaBytes)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}

	return body, contentType, nil
}

func extensionFor(contentType string) string {
	switch {
	case contentType == "image/jpeg":
		return "jpg"
	case contentType == "image/png":
		return "png"
	case contentType == "video/mp4":
		return "mp4"
	default:
		return "bin"
	}
}
