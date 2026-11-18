![Courier](.github/banner.svg)

Messaging gateway service for [RapidPro](https://rapidpro.io) and [TextIt](https://textit.com).

## Error reporting

Set `COURIER_SENTRY_DSN` to a GlitchTip/Sentry project DSN to report panics and
channel send failures. `COURIER_SENTRY_ENVIRONMENT` and `COURIER_SENTRY_RELEASE`
are optional tags; the release defaults to the build version.

Request data is never attached to events. Send failures are rate limited per
channel and error class, so a provider outage does not flood the project.
