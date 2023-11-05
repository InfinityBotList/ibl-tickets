package types

import "github.com/bwmarrin/discordgo"

type Attachment struct {
	ID          string   `json:"id"`           // ID of the attachment within the ticket
	URL         string   `json:"url"`          // URL of the attachment
	ProxyURL    string   `json:"proxy_url"`    // URL (cached) of the attachment
	Name        string   `json:"name"`         // Name of the attachment
	ContentType string   `json:"content_type"` // Content type of the attachment
	Size        int      `json:"size"`         // Size of the attachment in bytes
	Errors      []string `json:"errors"`       // Non-fatal errors that occurred while uploading the attachment
}

type Message struct {
	ID          string                    `json:"id"`
	Content     string                    `json:"content"`
	Embeds      []*discordgo.MessageEmbed `json:"embeds"`
	AuthorID    string                    `json:"author_id"`
	Attachments []Attachment              `json:"attachments"`
}

type FileTranscriptData struct {
	Issue         string            `json:"issue"`
	TopicID       string            `json:"topic_id"`
	Topic         Topic             `json:"topic"`
	TicketContext map[string]string `json:"ticket_context"`
	Messages      []Message         `json:"messages"`
	UserID        string            `json:"user_id"`
	CloseUserID   string            `json:"close_user_id"`
	ChannelID     string            `json:"channel_id"`
	TicketID      string            `json:"ticket_id"`
}
