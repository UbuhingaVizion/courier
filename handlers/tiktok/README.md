# TikTok channel

Receives TikTok Business Messaging webhooks at:

```
POST /c/tt/<channel-uuid>/receive
```

The channel type is `TT`. Each shop has its own TikTok developer app and channel, so the webhook URL is
per channel and a shared endpoint does not have to work out which business a delivery belongs to.

## Channel config

| Key | Purpose |
|---|---|
| `auth_token` | Short-lived access token from TikTok (`Access-Token` header on API calls). |
| `business_id` | The account's `open_id` from the token exchange; the `business_id` on every API call. |
| `client_key` | Developer app ID, used when refreshing tokens. |
| `client_secret` | Developer app secret. Also the key for webhook signatures. |
| `refresh_token` | Long-lived refresh token (one year). |
| `webhook_secret` | Optional. Used for signatures when it is set, otherwise `client_secret`. |

The access token lives for one day and the refresh token for one year. Courier does not refresh tokens; the
channel reports `40105` as a config error so the token can be refreshed and the channel reauthorized from the
RapidPro side. Rotate the refresh token when TikTok returns a new one.

## Identity

Inbound messages use the external scheme:

```
ext:tt_<sender_open_id>?conversation=<urlsafe-base64 conversation id>
```

The path is the sender's app-scoped `open_id`, so it stays the contact's identity. The query carries the
conversation the reply must go to. Conversation IDs contain `+`, `/` and `=`, which don't survive a URN query
unescaped, so the id is stored as unpadded base64url and decoded before sending.

The channel address is the business account's `open_id` (the `user_openid` on the webhook envelope), which is
different from the contact's.

## Signatures

Every delivery carries `TikTok-Signature: t=<unix seconds>,s=<hex>`. The signature is
`HMAC-SHA256(client_secret, t + "." + raw request body)`. The handler verifies it over the bytes as received,
before decoding, and rejects a delivery whose timestamp is more than five minutes old. Invalid or missing
signatures are answered with `403`.

## Message support

Inbound: text, image, video, shared posts, stickers, emoji. A sticker or emoji is passed through as a public
URL; image and video attachments are downloaded with the access token and stored as attachments, because
TikTok's temporary download URLs require an `x-user` header that mailroom does not send.

Outbound: text (split at TikTok's 6,000 character limit), images (JPG or PNG up to 3MB, uploaded first to get a
`media_id`), and Q&A button cards for up to three text quick replies. TikTok has no location pin and no
free-form buttons, so flows use numbered text replies there.

## Reply windows

TikTok only allows replies to users who messaged the business:

- after the first message, up to 10 replies in 48 hours;
- after each new message from the user, unlimited replies for the next 48 hours;
- after 48 hours of silence, 3 more messages until the user writes again.

There are no business-initiated messages and no templates that lift the window. A rejected message usually comes
back as code `40064`, which this handler treats as a permanent failure.

## Errors

| Condition | Courier error |
|---|---|
| HTTP 429 or code `40100` | `ErrConnectionThrottled` |
| HTTP 5xx or code `51065` | `ErrConnectionFailed` |
| codes `40001`, `40105` | `ErrChannelConfig` (reauthorize) |
| other non-zero codes | `ErrResponseContent` |

The API version is pinned in `apiBaseURL` in `handler.go`.
