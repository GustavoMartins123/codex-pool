// Package wire holds the provider-neutral intermediate representation (IR) of
// a conversation. Native response IDs, cache keys, signatures and sessions are
// provider-local state and must never enter the IR.
package wire

// Conversation is the provider-neutral conversation exchanged by handoff
// adapters. Native response IDs, cache keys, signatures, and sessions belong
// only to the provider session state.
type Conversation struct {
	System   []Content        `json:"system,omitempty"`
	Messages []Message        `json:"messages,omitempty"`
	Tools    []ToolDefinition `json:"tools,omitempty"`
	Metadata Metadata         `json:"metadata"`
}

// Role is the author of a message or content part.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentType discriminates the payload of a Content part.
type ContentType string

const (
	ContentText             ContentType = "text"
	ContentImage            ContentType = "image"
	ContentToolCall         ContentType = "tool_call"
	ContentToolResult       ContentType = "tool_result"
	ContentReasoningVisible ContentType = "reasoning_visible"
	ContentAttachment       ContentType = "attachment"
)

// Content is one portable part of a message.
type Content struct {
	Type      ContentType `json:"type"`
	Role      Role        `json:"role,omitempty"`
	Text      string      `json:"text,omitempty"`
	ToolID    string      `json:"tool_id,omitempty"`
	ToolName  string      `json:"tool_name,omitempty"`
	Arguments string      `json:"arguments,omitempty"`
	ImageURL  string      `json:"image_url,omitempty"`
	MimeType  string      `json:"mime_type,omitempty"`
	Data      string      `json:"data,omitempty"`
}

// Message is a single turn in the conversation.
type Message struct {
	Role    Role      `json:"role"`
	Content []Content `json:"content"`
}

// ToolDefinition is a portable tool schema; Parameters holds the raw JSON.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  string `json:"parameters,omitempty"`
}

// Metadata carries the conversation identity and transition epoch.
type Metadata struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Epoch          uint64 `json:"epoch,omitempty"`
}
