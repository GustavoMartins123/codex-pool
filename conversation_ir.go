package main

import (
	"codex-pool-proxy/internal/wire"
)

func conversationIRFromMessages(messages []Message, conversationID string, epoch uint64) wire.Conversation {
	ir := wire.Conversation{Metadata: wire.Metadata{ConversationID: conversationID, Epoch: epoch}}
	for _, message := range messages {
		content := make([]wire.Content, 0, len(message.Parts))
		for _, part := range message.Parts {
			content = append(content, wire.Content{
				Type: wire.ContentType(part.Type), Text: part.Text,
				ToolID: part.ToolID, ToolName: part.ToolName, Arguments: part.Arguments,
				ImageURL: part.ImageURL, MimeType: part.MimeType, Data: part.Data,
			})
		}
		if message.Role == "system" || message.Role == "developer" {
			for index := range content {
				content[index].Role = wire.Role(message.Role)
			}
			ir.System = append(ir.System, content...)
			continue
		}
		ir.Messages = append(ir.Messages, wire.Message{Role: wire.Role(message.Role), Content: content})
	}
	return ir
}

// irLegacyMessages projects the IR back onto the legacy handoff Message
// shape used by the conversation store.
func irLegacyMessages(ir wire.Conversation) []Message {
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

func irContentToMessagePart(part wire.Content) MessagePart {
	return MessagePart{
		Type: string(part.Type), Text: part.Text, ToolID: part.ToolID,
		ToolName: part.ToolName, Arguments: part.Arguments,
		ImageURL: part.ImageURL, MimeType: part.MimeType, Data: part.Data,
	}
}

func normalizeConversationIR(format contextWireFormat, object map[string]any, conversationID string, epoch uint64) wire.Conversation {
	return conversationIRFromMessages(normalizeConversationMessages(format, object), conversationID, epoch)
}

func renderConversationIR(format contextWireFormat, object map[string]any, ir wire.Conversation) {
	renderConversationMessages(format, object, irLegacyMessages(ir))
}
