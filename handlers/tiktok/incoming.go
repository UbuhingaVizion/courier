package tiktok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/nyaruka/courier/v26/core/channels"
	"github.com/nyaruka/courier/v26/core/models"
	"github.com/nyaruka/gocommon/svclogs"
	"github.com/nyaruka/gocommon/urns"
)

// handleEvent turns one TikTok webhook event into the messages courier writes. Events we understand but
// don't act on are reported as ignored so the response shows what arrived.
func (h *handler) handleEvent(ctx context.Context, ch *models.Channel, env *envelope, in *channels.Received, clog *models.ChannelLog) error {
	switch env.Event {
	case "im_receive_msg":
		// handled below
	case "im_receive_msg_eu":
		// TikTok strips the sender, conversation and body for accounts in the EEA, Switzerland and the UK.
		in.Ignored("im_receive_msg_eu has no sender or conversation")
		return nil
	case "im_send_msg":
		in.Ignored("echo of a message sent by the business")
		return nil
	default:
		in.Ignored(fmt.Sprintf("unsupported event: %s", env.Event))
		return nil
	}

	if env.Content == "" {
		in.Ignored("event has no content")
		return nil
	}

	var c messageContent
	if err := json.Unmarshal([]byte(env.Content), &c); err != nil {
		return fmt.Errorf("unable to parse event content: %w", err)
	}

	// the sender is the personal account in the conversation; user_openid is the business account
	if c.FromUser.Role != "personal_account" || c.FromUser.ID == "" {
		in.Ignored("event was not sent by a personal account")
		return nil
	}
	if c.ConversationID == "" {
		in.Ignored("event has no conversation id")
		return nil
	}

	urn, err := conversationURN(c.FromUser.ID, c.ConversationID)
	if err != nil {
		return fmt.Errorf("unable to build URN: %w", err)
	}

	receivedOn := time.Now()
	if c.Timestamp > 0 {
		receivedOn = time.UnixMilli(c.Timestamp)
	} else if env.CreateTime > 0 {
		receivedOn = time.Unix(env.CreateTime, 0)
	}

	msg := models.NewIncomingMsg(ch, urn, "", c.MessageID, clog).WithReceivedOn(receivedOn.UTC())

	switch c.Type {
	case "text":
		if c.Text != nil {
			msg.Text_ = c.Text.Body
		}

	case "share_post":
		if c.SharePost != nil {
			msg.Text_ = c.SharePost.EmbedURL
			if msg.Text_ == "" {
				msg.Text_ = c.SharePost.VideoID
			}
		}

	case "image", "video":
		mediaType := "IMAGE"
		if c.Type == "video" {
			mediaType = "VIDEO"
		}
		data, err := h.downloadMedia(ctx, ch, &c, mediaType, clog)
		if err != nil {
			clog.Error(&svclogs.Error{Message: fmt.Sprintf("unable to download %s: %s", c.Type, err)})
			in.Ignored(fmt.Sprintf("unable to download %s", c.Type))
			return nil
		}
		msg.WithAttachment("data:" + base64.StdEncoding.EncodeToString(data))

	case "sticker", "emoji":
		// these are public URLs that need no access token, so they can be fetched later
		if c.Sticker != nil && c.Sticker.URL != "" {
			msg.WithAttachment(c.Sticker.URL)
		} else if c.Emoji != nil && c.Emoji.URL != "" {
			msg.WithAttachment(c.Emoji.URL)
		}

	default:
		in.Ignored(fmt.Sprintf("unsupported message type: %s", c.Type))
		return nil
	}

	if msg.Text_ == "" && len(msg.Attachments()) == 0 {
		in.Ignored(fmt.Sprintf("nothing to handle in %s message", c.Type))
		return nil
	}

	in.Msg(msg)
	return nil
}

// conversationURN keeps the open_id as the URN path and carries the conversation id in the query so replies
// can address the conversation without changing the contact's identity. URN identity is scheme+path, which
// is why the conversation can be updated by later messages without creating a new contact.
func conversationURN(openID, conversationID string) (urns.URN, error) {
	// conversation ids are base64 and contain +, / and =, none of which survive a URN query unescaped. Store a
	// url-safe encoding of the id so the URN round-trips and the decoded value is what the send API expects.
	query := url.Values{}
	query.Set("conversation", base64.RawURLEncoding.EncodeToString([]byte(conversationID)))
	return urns.NewFromParts(urns.External.Prefix, "tt_"+openID, query, "")
}

// downloadMedia exchanges the message's media id for a temporary download URL and fetches the bytes. The
// download URL requires the access token in the x-user header, so it can't be handed to mailroom as a bare
// attachment; the bytes are returned inline instead.
func (h *handler) downloadMedia(ctx context.Context, ch *models.Channel, c *messageContent, mediaType string, clog *models.ChannelLog) ([]byte, error) {
	token := ch.StringConfigForKey(models.ConfigAuthToken, "")
	if token == "" {
		return nil, fmt.Errorf("channel is missing its access token")
	}

	businessID := ch.StringConfigForKey(configBusinessID, "")
	if businessID == "" {
		businessID = string(ch.Address())
	}
	if businessID == "" {
		return nil, fmt.Errorf("channel is missing its business id")
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
		return nil, fmt.Errorf("event has no media id")
	}

	payload, err := json.Marshal(downloadRequest{
		BusinessID:     businessID,
		ConversationID: c.ConversationID,
		MessageID:      c.MessageID,
		MediaID:        mediaID,
		MediaType:      mediaType,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/business/message/media/download/", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Token", token)

	resp, body, err := h.RequestHTTP(req, clog)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("download lookup returned HTTP %d", resp.StatusCode)
	}

	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unable to parse download response: %w", err)
	}
	if parsed.Code != 0 || parsed.Data.DownloadURL == "" {
		return nil, fmt.Errorf("download lookup failed with code %d: %s", parsed.Code, parsed.Message)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, parsed.Data.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	// TikTok requires the access token here as well, under a different header than the API calls
	req.Header.Set("x-user", token)

	resp, body, err = h.RequestHTTP(req, clog)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("media download returned HTTP %d", resp.StatusCode)
	}

	if len(body) > maxMediaBytes {
		return nil, fmt.Errorf("media exceeds %d bytes", maxMediaBytes)
	}

	return body, nil
}
