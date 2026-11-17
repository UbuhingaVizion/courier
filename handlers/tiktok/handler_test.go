package tiktok

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/nyaruka/courier/v26/core/models"
	. "github.com/nyaruka/courier/v26/handlers/handlertest"
	"github.com/nyaruka/courier/v26/runtime"
	"github.com/nyaruka/courier/v26/test"
	"github.com/nyaruka/courier/v26/web"
	"github.com/nyaruka/gocommon/httpx"
	"github.com/nyaruka/gocommon/urns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testChannel() *models.Channel {
	return test.NewMockChannel(
		"8eb23e93-5ecb-45ba-b726-3b064e0c568c", "TT", "business123", "",
		[]string{urns.External.Prefix},
		map[string]any{
			models.ConfigAuthToken: "token123",
			configClientSecret:     "secret123",
			configBusinessID:       "business123",
		},
	)
}

// signRequest signs a case the way TikTok does, so the signature check under test is the real one.
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

func TestSignature(t *testing.T) {
	ch := testChannel()
	h := &handler{}

	body := []byte(`{"event":"im_receive_msg"}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("secret123"))
	mac.Write([]byte(timestamp + "." + string(body)))
	valid := fmt.Sprintf("t=%s,s=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))

	assert.NoError(t, h.verifySignature(ch, valid, body))
	assert.Error(t, h.verifySignature(ch, "", body))
	assert.Error(t, h.verifySignature(ch, "nonsense", body))
	assert.Error(t, h.verifySignature(ch, fmt.Sprintf("t=%s,s=deadbeef", timestamp), body))

	// a correctly signed but stale delivery is rejected
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	mac = hmac.New(sha256.New, []byte("secret123"))
	mac.Write([]byte(stale + "." + string(body)))
	assert.Error(t, h.verifySignature(ch, fmt.Sprintf("t=%s,s=%s", stale, hex.EncodeToString(mac.Sum(nil))), body))

	// a channel with no secret fails closed
	unsigned := test.NewMockChannel("8eb23e93-5ecb-45ba-b726-3b064e0c568c", "TT", "business123", "", []string{urns.External.Prefix}, nil)
	assert.Error(t, h.verifySignature(unsigned, valid, body))
}

func TestIncoming(t *testing.T) {
	RunIncomingTests(t, []*models.Channel{testChannel()}, newHandler, "testdata/incoming.json", &IncomingOptions{
		Sign: signRequest("secret123"),
	})
}

func TestOutgoing(t *testing.T) {
	RunOutgoingTests(t, testChannel(), newHandler, "testdata/outgoing.json", &OutgoingOptions{
		CheckRedacted: []string{"token123", "secret123"},
		FreeURLs:      []string{apiBaseURL + "/business/message/media/upload/", "https://example.com/"},
	})
}

func TestConversationURN(t *testing.T) {
	urn, err := conversationURN("buyer123", "conv+1==")
	require.NoError(t, err)
	assert.Equal(t, "ext:tt_buyer123?conversation=Y29udisxPT0", string(urn))

	conversationID, err := conversationFromURN(urn)
	require.NoError(t, err)
	assert.Equal(t, "conv+1==", conversationID)
}

func TestDownloadMedia(t *testing.T) {
	s := web.NewServer(runtime.NewTestRuntime(runtime.NewDefaultConfig()))
	h := s.MountHandler(newHandler).(*handler)
	ch := testChannel()
	clog := models.NewChannelLog(models.ChannelLogTypeUnknown, ch, nil, h.RedactValues(ch))

	s.Runtime().HTTP.Default.Transport = test.MockTransport(map[string][]*httpx.MockResponse{
		apiBaseURL + "/business/message/media/download/": {
			httpx.NewMockResponse(200, nil, []byte(`{"code":0,"data":{"download_url":"https://cdn.example.com/image.jpg"}}`)),
		},
		"https://cdn.example.com/image.jpg": {
			httpx.NewMockResponse(200, map[string]string{"Content-Type": "image/jpeg"}, []byte("jpegdata")),
		},
	})

	content := &messageContent{
		ConversationID: "conv+1==",
		MessageID:      "msg1",
		Image:          &mediaPayload{MediaID: "media1"},
	}

	data, err := h.downloadMedia(context.Background(), ch, content, "IMAGE", clog)
	require.NoError(t, err)
	assert.Equal(t, []byte("jpegdata"), data)
}
