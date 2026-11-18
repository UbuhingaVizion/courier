package utils

import (
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

// Error reporting is rate limited per channel and error class. A provider outage produces one failed send per
// message, and without this a single bad channel would fill the project with duplicate events.
const (
	sentryWindow       = time.Minute
	sentryMaxPerWindow = 3
)

type sentryBucket struct {
	start time.Time
	count int
}

var (
	sentryMu      sync.Mutex
	sentryBuckets = map[string]*sentryBucket{}
	sentryNow     = time.Now
)

// SentryBeforeSend keeps request bodies, headers and query strings out of events. Courier handlers handle
// phone numbers and message text, and none of that belongs in an error report.
func SentryBeforeSend(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
	event.Request = nil
	return event
}

func allowSentryEvent(key string) bool {
	sentryMu.Lock()
	defer sentryMu.Unlock()

	now := sentryNow()
	bucket := sentryBuckets[key]
	if bucket == nil || now.Sub(bucket.start) > sentryWindow {
		sentryBuckets[key] = &sentryBucket{start: now, count: 1}
		return true
	}
	if bucket.count >= sentryMaxPerWindow {
		return false
	}
	bucket.count++
	return true
}

func resetSentryLimiter() {
	sentryMu.Lock()
	defer sentryMu.Unlock()
	sentryBuckets = map[string]*sentryBucket{}
}

// ReportError captures an unexpected error with tags describing where it happened.
func ReportError(err error, tags map[string]string) {
	if err == nil {
		return
	}
	if !allowSentryEvent(tags["channel_uuid"] + ":" + tags["error_class"]) {
		return
	}

	sentry.WithScope(func(scope *sentry.Scope) {
		setSentryTags(scope, tags)
		scope.SetFingerprint([]string{"courier", tags["channel_type"], tags["error_class"]})
		sentry.CaptureException(err)
	})
}

// ReportSendError captures a channel send failure by its courier error class. The provider's raw response is not
// included because it can contain message text or phone numbers.
func ReportSendError(class, message string, retryable bool, tags map[string]string) {
	if !allowSentryEvent(tags["channel_uuid"] + ":" + class) {
		return
	}

	level := sentry.LevelError
	if retryable {
		level = sentry.LevelWarning
	}

	sentry.WithScope(func(scope *sentry.Scope) {
		setSentryTags(scope, tags)
		scope.SetTag("error_class", class)
		scope.SetTag("retryable", boolString(retryable))
		scope.SetLevel(level)
		scope.SetFingerprint([]string{"courier", "send", tags["channel_type"], class})
		sentry.CaptureMessage(message)
	})
}

func setSentryTags(scope *sentry.Scope, tags map[string]string) {
	scope.SetTag("service", "courier")
	for key, value := range tags {
		if value != "" {
			scope.SetTag(key, value)
		}
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
