package tiktok

type envelope struct {
	ClientKey  string `json:"client_key"`
	Event      string `json:"event"`
	CreateTime int64  `json:"create_time"`
	UserOpenID string `json:"user_openid"`
	Content    string `json:"content"`
}

type messageContent struct {
	From             string        `json:"from"`
	To               string        `json:"to"`
	UniqueIdentifier string        `json:"unique_identifier"`
	FromUser         participant   `json:"from_user"`
	ToUser           participant   `json:"to_user"`
	ConversationID   string        `json:"conversation_id"`
	MessageID        string        `json:"message_id"`
	Timestamp        int64         `json:"timestamp"` // unix milliseconds
	Type             string        `json:"type"`      // lowercase in webhooks
	Text             *textPayload  `json:"text"`
	Image            *mediaPayload `json:"image"`
	Video            *mediaPayload `json:"video"`
	SharePost        *sharePayload `json:"share_post"`
	Sticker          *urlPayload   `json:"sticker"`
	Emoji            *urlPayload   `json:"emoji"`
	Template         *templateBody `json:"template"`
	ReplySource      *replySource  `json:"reply_source_payload"`
	AutoMessageType  string        `json:"auto_message_type"`
	IsFollower       bool          `json:"is_follower"`
}

type participant struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

type textPayload struct {
	Body string `json:"body"`
}

type mediaPayload struct {
	MediaID string `json:"media_id"`
}

type sharePayload struct {
	EmbedURL string `json:"embed_url"`
	VideoID  string `json:"video_id"`
}

type urlPayload struct {
	URL string `json:"url"`
}

type templateBody struct {
	Type     string            `json:"type"`
	Elements []templateElement `json:"elements"`
}

type templateElement struct {
	Title   string           `json:"title"`
	Buttons []templateButton `json:"buttons"`
}

type templateButton struct {
	Text string `json:"text"`
	Type string `json:"type"`
	ID   string `json:"id"`
}

type replySource struct {
	ReplySourceMsgID  string `json:"reply_source_msg_id"`
	ReplySourceUnique string `json:"reply_source_unique_id"`
}

// outbound templates use a flat title and buttons with a title field rather than the inbound elements shape.
type outboundTemplate struct {
	Type    string           `json:"type"`
	Title   string           `json:"title"`
	Buttons []outboundButton `json:"buttons"`
}

type outboundButton struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	ID    string `json:"id"`
}

type sendRequest struct {
	BusinessID    string            `json:"business_id"`
	RecipientType string            `json:"recipient_type,omitempty"`
	Recipient     string            `json:"recipient,omitempty"`
	MessageType   string            `json:"message_type"`
	Text          *textPayload      `json:"text,omitempty"`
	Image         *imageBody        `json:"image,omitempty"`
	Template      *outboundTemplate `json:"template,omitempty"`
	SenderAction  string            `json:"sender_action,omitempty"`
}

type imageBody struct {
	MediaID string `json:"media_id"`
}

type apiResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Message struct {
			MessageID string `json:"message_id"`
		} `json:"message"`
		MediaID     string `json:"media_id"`
		DownloadURL string `json:"download_url"`
	} `json:"data"`
}

type downloadRequest struct {
	BusinessID     string `json:"business_id"`
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	MediaID        string `json:"media_id"`
	MediaType      string `json:"media_type"`
}
