package main

// ConversationIR is the provider-neutral conversation exchanged by handoff
// adapters. Native response IDs, cache keys, signatures, and sessions belong
// only to ProviderSessionState.
type ConversationIR struct {
	System   []IRContent        `json:"system,omitempty"`
	Messages []IRMessage        `json:"messages,omitempty"`
	Tools    []IRToolDefinition `json:"tools,omitempty"`
	Metadata IRMetadata         `json:"metadata"`
}

type IRRole string

const (
	IRRoleSystem    IRRole = "system"
	IRRoleDeveloper IRRole = "developer"
	IRRoleUser      IRRole = "user"
	IRRoleAssistant IRRole = "assistant"
	IRRoleTool      IRRole = "tool"
)

type IRContentType string

const (
	IRText             IRContentType = "text"
	IRImage            IRContentType = "image"
	IRToolCall         IRContentType = "tool_call"
	IRToolResult       IRContentType = "tool_result"
	IRReasoningVisible IRContentType = "reasoning_visible"
	IRAttachment       IRContentType = "attachment"
)

type IRContent struct {
	Type      IRContentType `json:"type"`
	Role      IRRole        `json:"role,omitempty"`
	Text      string        `json:"text,omitempty"`
	ToolID    string        `json:"tool_id,omitempty"`
	ToolName  string        `json:"tool_name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
	ImageURL  string        `json:"image_url,omitempty"`
	MimeType  string        `json:"mime_type,omitempty"`
	Data      string        `json:"data,omitempty"`
}

type IRMessage struct {
	Role    IRRole      `json:"role"`
	Content []IRContent `json:"content"`
}

type IRToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  string `json:"parameters,omitempty"`
}

type IRMetadata struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Epoch          uint64 `json:"epoch,omitempty"`
}

func conversationIRFromMessages(messages []Message, conversationID string, epoch uint64) ConversationIR {
	ir := ConversationIR{Metadata: IRMetadata{ConversationID: conversationID, Epoch: epoch}}
	for _, message := range messages {
		content := make([]IRContent, 0, len(message.Parts))
		for _, part := range message.Parts {
			content = append(content, IRContent{
				Type: IRContentType(part.Type), Text: part.Text,
				ToolID: part.ToolID, ToolName: part.ToolName, Arguments: part.Arguments,
				ImageURL: part.ImageURL, MimeType: part.MimeType, Data: part.Data,
			})
		}
		if message.Role == "system" || message.Role == "developer" {
			for index := range content {
				content[index].Role = IRRole(message.Role)
			}
			ir.System = append(ir.System, content...)
			continue
		}
		ir.Messages = append(ir.Messages, IRMessage{Role: IRRole(message.Role), Content: content})
	}
	return ir
}

func (ir ConversationIR) legacyMessages() []Message {
	var messages []Message
	for _, part := range ir.System {
		role := string(part.Role)
		if role != "developer" {
			role = "system"
		}
		messages = append(messages, Message{Role: role, Parts: []MessagePart{irContentToMessagePart(part)}})
	}
	for _, value := range ir.Messages {
		message := Message{Role: string(value.Role)}
		for _, part := range value.Content {
			message.Parts = append(message.Parts, irContentToMessagePart(part))
		}
		messages = append(messages, message)
	}
	return messages
}

func irContentToMessagePart(part IRContent) MessagePart {
	return MessagePart{
		Type: string(part.Type), Text: part.Text, ToolID: part.ToolID,
		ToolName: part.ToolName, Arguments: part.Arguments,
		ImageURL: part.ImageURL, MimeType: part.MimeType, Data: part.Data,
	}
}

func normalizeConversationIR(format contextWireFormat, object map[string]any, conversationID string, epoch uint64) ConversationIR {
	return conversationIRFromMessages(normalizeConversationMessages(format, object), conversationID, epoch)
}

func renderConversationIR(format contextWireFormat, object map[string]any, ir ConversationIR) {
	renderConversationMessages(format, object, ir.legacyMessages())
}
