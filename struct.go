package poe_api_go

type SubscriptionsMutation struct {
	Subscriptions []Subscription `json:"subscriptions"`
}

type Subscription struct {
	SubscriptionName string      `json:"subscriptionName"`
	Query            interface{} `json:"query"`
	QueryHash        string      `json:"queryHash"`
}

type Payload struct {
	QueryName  string                 `json:"queryName"`
	Variables  interface{}            `json:"variables"`
	Extensions map[string]interface{} `json:"extensions"`
}

type Message struct {
	ChatID        float64       `json:"chatId"`
	Bot           string        `json:"bot"`
	Query         string        `json:"query"`
	Source        Source        `json:"source"`
	WithChatBreak bool          `json:"withChatBreak"`
	ClientNonce   string        `json:"clientNonce"`
	Sdid          string        `json:"sdid"`
	Attachments   []interface{} `json:"attachments"`
}

type Source struct {
	SourceType        string                 `json:"sourceType"`
	ChatInputMetadata map[string]interface{} `json:"chatInputMetadata"`
}
