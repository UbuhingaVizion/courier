package utils

import (
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/stretchr/testify/assert"
)

func TestAllowSentryEvent(t *testing.T) {
	resetSentryLimiter()

	now := time.Now()
	sentryNow = func() time.Time { return now }
	defer func() { sentryNow = time.Now }()

	for i := 0; i < sentryMaxPerWindow; i++ {
		assert.True(t, allowSentryEvent("channel:rate_limited"))
	}
	assert.False(t, allowSentryEvent("channel:rate_limited"))
	assert.True(t, allowSentryEvent("other:rate_limited"))

	now = now.Add(sentryWindow + time.Second)
	assert.True(t, allowSentryEvent("channel:rate_limited"))
}

func TestSentryBeforeSendDropsRequest(t *testing.T) {
	event := &sentry.Event{Request: &sentry.Request{URL: "https://example.com/c/tt/123/receive"}}

	assert.Nil(t, SentryBeforeSend(event, nil).Request)
}
