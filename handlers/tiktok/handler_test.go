package tiktok

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/nyaruka/courier"
	. "github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/test"
	"github.com/nyaruka/gocommon/httpx"
	"github.com/nyaruka/gocommon/urns"
	"github.com/stretchr/testify/assert"
)

const (
	testChannelUUID = "8eb23e93-5ecb-45ba-b726-3b064e0c568c"
	testSecret      = "secret123"
)

func testChannel() courier.Channel {
	return test.NewMockChannel(testChannelUUID, "TT", "business123", "", map[string]any{
		courier.ConfigAuthToken: "token123",
		configClientSecret:      testSecret,
		configBusinessID:        "business123",
	})
}

func signRequest(secret string) RequestPrepFunc {
	return func(r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(timestamp + "." + string(body)))

		r.Header.Set(signatureHeader, fmt.Sprintf("t=%s,s=%s", timestamp, hex.EncodeToString(mac.Sum(nil))))
	}
}

func envelopeJSON(event, content string) string {
	payload, _ := json.Marshal(map[string]any{
		"client_key":  "app123",
		"event":       event,
		"create_time": 1714430514,
		"user_openid": "business123",
		"content":     content,
	})
	return string(payload)
}

func textContent(messageID, text string) string {
	content, _ := json.Marshal(map[string]any{
		"from":            "buyer",
		"to":              "shop",
		"from_user":       map[string]any{"id": "buyer123", "role": "personal_account"},
		"to_user":         map[string]any{"id": "business123", "role": "business_account"},
		"conversation_id": "conv+1==",
		"message_id":      messageID,
		"timestamp":       1714430514000,
		"type":            "text",
		"text":            map[string]any{"body": text},
	})
	return string(content)
}

func TestSignature(t *testing.T) {
	channel := testChannel()
	h := &handler{}

	body := []byte(`{"event":"im_receive_msg"}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(timestamp + "." + string(body)))
	valid := fmt.Sprintf("t=%s,s=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))

	assert.NoError(t, h.verifySignature(channel, valid, body))
	assert.Error(t, h.verifySignature(channel, "", body))
	assert.Error(t, h.verifySignature(channel, "nonsense", body))

	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	mac = hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(stale + "." + string(body)))
	assert.Error(t, h.verifySignature(channel, fmt.Sprintf("t=%s,s=%s", stale, hex.EncodeToString(mac.Sum(nil))), body))

	unsigned := test.NewMockChannel(testChannelUUID, "TT", "business123", "", nil)
	assert.Error(t, h.verifySignature(unsigned, valid, body))
}

func TestConversationURN(t *testing.T) {
	urn, err := conversationURN("buyer123", "conv+1==")
	assert.NoError(t, err)
	assert.Equal(t, "ext:tt_buyer123?conversation=Y29udisxPT0", string(urn))

	conversationID, err := conversationFromURN(urn)
	assert.NoError(t, err)
	assert.Equal(t, "conv+1==", conversationID)
}

func TestIncoming(t *testing.T) {
	text := "ORDER SKU-1"
	received := time.UnixMilli(1714430514000)

	cases := []IncomingTestCase{
		{
			Label:              "Receive Text",
			URL:                "/c/tt/" + testChannelUUID + "/receive",
			Data:               envelopeJSON("im_receive_msg", textContent("msg1", text)),
			PrepRequest:        signRequest(testSecret),
			ExpectedRespStatus: 200,
			ExpectedMsgText:    &text,
			ExpectedURN:        urns.URN("ext:tt_buyer123?conversation=Y29udisxPT0"),
			ExpectedExternalID: "msg1",
			ExpectedDate:       received,
		},
		{
			Label:              "Receive Share Post",
			URL:                "/c/tt/" + testChannelUUID + "/receive",
			Data:               envelopeJSON("im_receive_msg", `{"from_user":{"id":"buyer123","role":"personal_account"},"to_user":{"id":"business123","role":"business_account"},"conversation_id":"conv+1==","message_id":"msg2","timestamp":1714430515000,"type":"share_post","share_post":{"embed_url":"https://www.tiktok.com/@shop/video/123","video_id":"123"}}`),
			PrepRequest:        signRequest(testSecret),
			ExpectedRespStatus: 200,
			ExpectedMsgText:    stringPtr("https://www.tiktok.com/@shop/video/123"),
			ExpectedURN:        urns.URN("ext:tt_buyer123?conversation=Y29udisxPT0"),
			ExpectedExternalID: "msg2",
		},
		{
			Label:                "Ignore Echo",
			URL:                  "/c/tt/" + testChannelUUID + "/receive",
			Data:                 envelopeJSON("im_send_msg", textContent("msg3", "Hello")),
			PrepRequest:          signRequest(testSecret),
			ExpectedRespStatus:   200,
			ExpectedBodyContains: "Ignored",
		},
		{
			Label:                "Reject Invalid Signature",
			URL:                  "/c/tt/" + testChannelUUID + "/receive",
			Data:                 envelopeJSON("im_receive_msg", textContent("msg4", "Hello")),
			Headers:              map[string]string{signatureHeader: "t=1714430514,s=deadbeef"},
			ExpectedRespStatus:   403,
			ExpectedBodyContains: "invalid request signature",
		},
	}

	RunIncomingTestCases(t, []courier.Channel{testChannel()}, newHandler(), cases)
}

func TestOutgoing(t *testing.T) {
	sendResponse := httpx.NewMockResponse(200, nil, []byte(`{"code":0,"message":"OK","data":{"message":{"message_id":"mid1"}}}`))

	cases := []OutgoingTestCase{
		{
			Label:   "Send Text",
			MsgText: "Your order TUZ-10245 is confirmed.",
			MsgURN:  "ext:tt_buyer123?conversation=Y29udisxPT0",
			MockResponses: map[string][]*httpx.MockResponse{
				"*/business/message/send/": {sendResponse},
			},
			ExpectedRequests: []ExpectedRequest{
				{Path: "/open_api/v1.3/business/message/send/", BodyContains: `"message_type":"TEXT"`, Headers: map[string]string{"Access-Token": "token123"}},
			},
			ExpectedExtIDs: []string{"mid1"},
		},
		{
			Label:           "Send Quick Replies",
			MsgText:         "How would you like to pay?",
			MsgURN:          "ext:tt_buyer123?conversation=Y29udisxPT0",
			MsgQuickReplies: []string{"Cash on delivery", "M-Pesa"},
			MockResponses: map[string][]*httpx.MockResponse{
				"*/business/message/send/": {
					httpx.NewMockResponse(200, nil, []byte(`{"code":0,"message":"OK","data":{"message":{"message_id":"mid2"}}}`)),
				},
			},
			ExpectedRequests: []ExpectedRequest{
				{Path: "/open_api/v1.3/business/message/send/", BodyContains: `"title":"How would you like to pay?"`},
			},
			ExpectedExtIDs: []string{"mid2"},
		},
		{
			Label:          "Send Image",
			MsgURN:         "ext:tt_buyer123?conversation=Y29udisxPT0",
			MsgAttachments: []string{"image/jpeg:https://example.com/image.jpg"},
			MockResponses: map[string][]*httpx.MockResponse{
				"https://example.com/image.jpg": {
					httpx.NewMockResponse(200, map[string]string{"Content-Type": "image/jpeg"}, []byte("jpeg-bytes")),
				},
				"*/business/message/media/upload/": {
					httpx.NewMockResponse(200, nil, []byte(`{"code":0,"message":"OK","data":{"media_id":"media123"}}`)),
				},
				"*/business/message/send/": {
					httpx.NewMockResponse(200, nil, []byte(`{"code":0,"message":"OK","data":{"message":{"message_id":"mid3"}}}`)),
				},
			},
			ExpectedRequests: []ExpectedRequest{
				{Path: "/image.jpg"},
				{Path: "/open_api/v1.3/business/message/media/upload/"},
				{Path: "/open_api/v1.3/business/message/send/", BodyContains: `"media_id":"media123"`},
			},
			ExpectedExtIDs: []string{"mid3"},
		},
	}

	RunOutgoingTestCases(t, testChannel(), newHandler(), cases, []string{"auth_token", "client_secret"}, nil)
}

func stringPtr(s string) *string { return &s }
