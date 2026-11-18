// Package channel defines the pluggable IM adapter interface.
//
// A Channel is an adapter for one chat platform (Feishu, DingTalk, WeCom,
// Telegram, Discord, Slack, ...). It ingests inbound chat messages and
// pushes them onto the bus; outbound messages flow back through a Sender
// bound to the originating chat.
package channel

import (
	"context"
	"net/http"
	"time"
)

// Attachment is a file/image/voice attached to an inbound message.
type Attachment struct {
	Type string // "image" | "file" | "audio" | "video"
	Name string
	URL  string // remote URL if any
	Path string // local path if the channel already downloaded it
	Size int64
}

// Message is the normalized inbound chat message, produced by every
// channel adapter and consumed by the router/agent runtime.
type Message struct {
	ID          string        // platform message id (for reply/quote/react)
	Channel     string        // channel name, e.g. "feishu"
	ChatID      string        // conversation id, stable per chat/group
	ChatType    string        // "p2p" | "group"
	ThreadID    string        // thread/topic root id, "" for flat chats
	UserID      string        // sender id on the platform
	UserName    string        // display name, best effort
	Text        string        // normalized plain text, mentions stripped
	MentionedMe bool          // true if the bot was @-mentioned
	AtUserIDs   []string      // user ids @-mentioned in the text (excluding me)
	Attachments []Attachment
	Timestamp   time.Time
	Raw         any // platform-native event, for adapters/debug only
}

// SendOptions tweak an outbound message.
type SendOptions struct {
	ReplyTo        string   // quote/reply to this message id
	MentionUserIDs []string // @ these platform user ids in the text
	Final          bool     // true: final answer; false: interim progress
}

// Sender sends outbound messages into one specific chat.
type Sender interface {
	// Send posts a new message and returns its platform message id.
	Send(ctx context.Context, text string, opts SendOptions) (string, error)
	// Replace edits a previously sent message in place (for streaming
	// updates). Channels without edit support may resend or drop.
	Replace(ctx context.Context, msgID, text string) error
	// React adds an emoji reaction to a message (best effort).
	React(ctx context.Context, msgID, emoji string) error
}

// Channel is one IM platform adapter instance.
type Channel interface {
	Name() string
	// Start connects (websocket/polling/webhook server) and pushes
	// inbound messages to out. It must return only when ctx is done
	// or a fatal error occurs; transient errors are retried internally.
	Start(ctx context.Context, out chan<- Message) error
	// SenderFor returns a Sender bound to the chat that msg came from.
	SenderFor(msg Message) (Sender, error)
	// Healthy reports connectivity for /healthz.
	Healthy() bool
}

// HTTPMounter is implemented by webhook-based channels (WeCom, Feishu
// callback mode, ...): endpoints mounted on the shared HTTP server.
type HTTPMounter interface {
	Endpoints() map[string]http.Handler
}
