# TikTok channel (v9.1.19 line)

Receives TikTok Business Messaging webhooks at:

```
POST /c/tt/<channel-uuid>/receive
```

This is the v9.1.19-compatible handler used by the RapidPro stack. The v26
branch has the same behaviour on the newer handler API.

## Channel config

| Key | Purpose |
|---|---|
| `auth_token` | Short-lived access token from TikTok (`Access-Token` header on API calls). |
| `business_id` | The account's `open_id` from the token exchange. |
| `client_key` | Developer app ID, used when refreshing tokens. |
| `client_secret` | Developer app secret. Also the key for webhook signatures. |
| `refresh_token` | Long-lived refresh token. |
| `webhook_secret` | Optional; used for signatures when set, otherwise `client_secret`. |

Courier does not refresh tokens. An expired token comes back as code `40105`
and is reported as a channel config error so the channel can be reauthorized.

## Identity

```
ext:tt_<sender_open_id>?conversation=<urlsafe-base64 conversation id>
```

The path is the sender's app-scoped `open_id`; the query carries the
conversation a reply must go to. Conversation ids contain `+`, `/` and `=`,
which do not survive a URN query unescaped, so the id is stored as unpadded
base64url and decoded before sending.

The channel address is the business account's `open_id`, which is different
from the contact's.

## Signatures

`TikTok-Signature: t=<unix seconds>,s=<hex>` where the signature is
`HMAC-SHA256(signing secret, t + "." + raw request body)`. The handler verifies
the bytes as received before decoding and rejects a delivery older than five
minutes. Invalid or missing signatures are answered with `403`.

## Message support

Inbound: text, image, video, shared posts, stickers, emoji. Media is downloaded
with the access token, stored through the backend, and added as an attachment.

Outbound: text (split at 6,000 characters), images (JPG/PNG up to 3MB, uploaded
first for a media id), and Q&A button cards for up to three quick replies.

## Reply windows

TikTok only allows replies to users who messaged the business: up to 10 replies
in the 48 hours after the first message, unlimited after each new user message,
then 3 more after 48 hours of silence. There are no business-initiated messages.
