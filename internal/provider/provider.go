// Package provider defines the model-calling layer: direct chat-completion
// APIs (OpenAI-compatible, Anthropic) that agents use to think.
package provider

import "context"

// Role of a chat message.
type Role string

const (
	System    Role = "system"
	User      Role = "user"
	Assistant Role = "assistant"
)

// Msg is one turn of conversation history.
type Msg struct {
	Role    Role     `json:"role"`
	Content string   `json:"content"`
	Name    string   `json:"name,omitempty"`   // speaker display name, group chats
	Images  []string `json:"images,omitempty"` // http(s) URLs or local paths
}

// Request is a model call.
type Request struct {
	Model       string
	System      string
	Messages    []Msg
	MaxTokens   int
	Temperature float64
	TopP        float64
	// APIKey overrides the provider-level key (per-agent keys).
	APIKey string
	// BaseURL overrides the provider-level base URL (OpenAI-compatible
	// gateways: DeepSeek, Moonshot, vLLM, one-api, ...).
	BaseURL string
}

// Delta is a streamed piece of a model response. Exactly one of Text or
// Err is meaningful; a Delta with Done=true ends the stream.
type Delta struct {
	Text string
	// Usage filled on the final delta when the API reports it.
	InputTokens  int
	OutputTokens int
	Done         bool
	Err          error
}

// Provider is a chat-completion backend.
type Provider interface {
	Name() string
	// Stream issues a model call and returns a channel of deltas.
	// The channel closes after the final delta. Cancelling ctx aborts.
	Stream(ctx context.Context, req Request) (<-chan Delta, error)
}

// Registry keeps named providers for config-driven agent wiring.
type Registry struct{ byName map[string]Provider }

func NewRegistry() *Registry { return &Registry{byName: map[string]Provider{}} }

func (r *Registry) Register(p Provider) { r.byName[p.Name()] = p }

func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}
