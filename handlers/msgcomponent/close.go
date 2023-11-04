package msgcomponent

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"ibl-tickets/types"
	"ibl-tickets/utils"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/infinitybotlist/eureka/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
	jsoniter "github.com/json-iterator/go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

var json = jsoniter.ConfigFastest

func _createAttachmentBlob(logger *zap.Logger, msg *discordgo.Message) ([]types.Attachment, map[string]*bytes.Buffer, error) {
	var attachments []types.Attachment
	var bufs = map[string]*bytes.Buffer{}
	for _, attachment := range msg.Attachments {
		if attachment.Size > 16_000_000 {
			attachments = append(attachments, types.Attachment{
				ID:       attachment.ID,
				Name:     attachment.Filename,
				URL:      attachment.URL,
				ProxyURL: attachment.ProxyURL,
				Errors:   []string{"Attachment is too large to be uploaded to the transcript."},
			})
			continue
		}

		// Download the attachment
		var url string

		if attachment.ProxyURL != "" {
			url = attachment.ProxyURL
		} else {
			url = attachment.URL
		}

		resp, err := http.Get(url)

		if err != nil {
			logger.Error("Error downloading attachment", zap.Error(err), zap.String("url", url))
			return attachments, nil, fmt.Errorf("error downloading attachment: %w", err)
		}

		bt, err := io.ReadAll(resp.Body)

		if err != nil {
			logger.Error("Error reading attachment", zap.Error(err), zap.String("url", url))
			return attachments, nil, fmt.Errorf("error reading attachment: %w", err)
		}

		bufs[attachment.ID] = bytes.NewBuffer(bt)

		attachments = append(attachments, types.Attachment{
			ID:     attachment.ID,
			Name:   attachment.Filename,
			Errors: []string{},
		})
	}

	return attachments, bufs, nil
}

func close(s *discordgo.Session, i *discordgo.Interaction, data discordgo.MessageComponentInteractionData, config *types.Config, pool *pgxpool.Pool, ctx context.Context, logger *zap.Logger, rediscli *redis.Client) error {
	tikId := strings.Split(data.CustomID, ":")[1]

	// Get the open tickets channel ID
	var ticketsChannelId string
	var open bool
	var userId string
	var issue string
	var topicId string
	var ticketContext map[string]string

	tx, err := pool.Begin(ctx)

	if err != nil {
		logger.Error("Error starting transaction", zap.Error(err))
		return err
	}

	err = tx.QueryRow(ctx, "SELECT issue, topic_id, user_id, channel_id, open, ticket_context FROM tickets WHERE id = $1", tikId).Scan(&issue, &topicId, &userId, &ticketsChannelId, &open, &ticketContext)

	if err != nil {
		logger.Error("Error getting ticket", zap.Error(err), zap.String("ticket_id", tikId))
		return s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "An error occurred while finding this ticket. Please contact our support team about this!",
				Flags:   discordgo.MessageFlagsEphemeral,
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})
	}

	if ticketsChannelId != i.ChannelID {
		return s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "You can't close a ticket that isn't in this channel!",
				Flags:   discordgo.MessageFlagsEphemeral,
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})
	}

	if !open {
		return s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This ticket is already closed?!",
				Flags:   discordgo.MessageFlagsEphemeral,
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})
	}

	// Start closing ticket
	s.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Closing ticket " + tikId + "... Please wait...",
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		},
	})

	topic, ok := config.Topics[topicId]

	if !ok {
		return fmt.Errorf("invalid topic id: %s", topicId)
	}

	// Update the database setting open to false
	_, err = tx.Exec(ctx, "UPDATE tickets SET open = false, close_user_id = $2 WHERE id = $1", tikId, i.Member.User.ID)

	if err != nil {
		logger.Error("Error closing ticket", zap.Error(err), zap.String("ticket_id", tikId))
		_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("An error occurred while closing this ticket. Please contact our support team about this!"),
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		})
		return err
	}

	// Collect every message in the channel
	var messages []types.Message

	var lastMessageId string
	attachmentBuf := map[string]*bytes.Buffer{}
	for {
		msgs, err := s.ChannelMessages(ticketsChannelId, 100, lastMessageId, "", "")

		if err != nil {
			logger.Error("Error getting messages", zap.Error(err), zap.String("ticket_id", tikId))

			// Send a message to the user
			_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
				Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't find messages)! Please try again later.\nlastMessageId=" + lastMessageId),
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			})
			return err
		}

		for _, msg := range msgs {
			attachments, bufs, err := _createAttachmentBlob(logger, msg)

			if err != nil {
				return fmt.Errorf("error creating attachment blob: %w", err)
			}

			for k, v := range bufs {
				attachmentBuf[k] = v
			}

			messages = append(messages, types.Message{
				ID:          msg.ID,
				AuthorID:    msg.Author.ID,
				Content:     msg.Content,
				Embeds:      msg.Embeds,
				Attachments: attachments,
			})
		}

		if len(msgs) < 100 {
			break
		}

		lastMessageId = msgs[len(msgs)-1].ID
	}

	// Update database with the messages
	_, err = tx.Exec(ctx, "UPDATE tickets SET messages = $1 WHERE id = $2", messages, tikId)

	if err != nil {
		logger.Error("Error updating ticket with messages", zap.Error(err), zap.String("ticket_id", tikId))

		// Send a message to the user
		newmsg := "Your ticket couldn't be closed properly (couldn't update database)! Please try again later."
		_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: &newmsg,
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		})
		return err
	}

	// If we have attachments, upload them to FileStoragePath
	if len(attachmentBuf) > 0 {
		logger.Info("Uploading attachments", zap.Int("count", len(attachmentBuf)), zap.String("ticket_id", tikId))

		// Delete FileStoragePath/{tikId} folder if it exists
		err = os.RemoveAll(config.Database.FileStoragePath + "/" + tikId)

		if err != nil {
			logger.Error("Error removing folder", zap.Error(err), zap.String("ticket_id", tikId))

			// Send a message to the user
			_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
				Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't remove folder)! Please try again later."),
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			})
			return err
		}

		// Make the FileStoragePath/{tikId} folder
		err = os.MkdirAll(config.Database.FileStoragePath+"/"+tikId, 0775)

		if err != nil {
			logger.Error("Error creating folder", zap.Error(err), zap.String("ticket_id", tikId))

			// Send a message to the user
			_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
				Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't create folder)! Please try again later."),
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			})
			return err
		}

		encKey := crypto.RandString(4096)

		keyHash := sha256.New()
		keyHash.Write([]byte(encKey))

		_, err = tx.Exec(ctx, "UPDATE tickets SET enc_key = $1 WHERE id = $2", encKey, tikId)

		if err != nil {
			logger.Error("Error updating ticket with enc_key", zap.Error(err), zap.String("ticket_id", tikId))

			// Send a message to the user
			_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
				Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't update database with enc_key)! Please try again later."),
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			})
			return err
		}

		for k, v := range attachmentBuf {
			// AES512-GCM encrypt the attachment
			c, err := aes.NewCipher(keyHash.Sum(nil))

			if err != nil {
				logger.Error("Error creating cipher", zap.Error(err), zap.String("ticket_id", tikId))

				// Send a message to the user
				_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
					Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't create cipher)! Please try again later."),
					AllowedMentions: &discordgo.MessageAllowedMentions{
						Parse: []discordgo.AllowedMentionType{},
					},
				})
				return err
			}

			gcm, err := cipher.NewGCM(c)

			if err != nil {
				return err
			}

			aesNonce := make([]byte, gcm.NonceSize())
			if _, err = io.ReadFull(rand.Reader, aesNonce); err != nil {
				return err
			}

			data := gcm.Seal(aesNonce, aesNonce, v.Bytes(), nil)

			// Save to FileStoragePath/{tikId}/{attachmentId}.encBlob
			err = os.WriteFile(config.Database.FileStoragePath+"/"+tikId+"/"+k+".encBlob", data, 0775)

			if err != nil {
				logger.Error("Error writing file", zap.Error(err), zap.String("ticket_id", tikId))

				// Send a message to the user
				_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
					Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't write file)! Please try again later."),
					AllowedMentions: &discordgo.MessageAllowedMentions{
						Parse: []discordgo.AllowedMentionType{},
					},
				})
				return err
			}
		}
	}

	ticketUrl := config.Database.ExposedPath + "/tickets/" + tikId

	// Send transcript to ticket thread channel and to user
	embed := &discordgo.MessageEmbed{
		Title: "Ticket Closed",
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Ticket ID",
				Value:  tikId,
				Inline: false,
			},
			{
				Name:   "User",
				Value:  "<@" + userId + ">",
				Inline: false,
			},
			{
				Name:   "Closed By",
				Value:  i.Member.Mention(),
				Inline: false,
			},
			{
				Name:   "Ticket URL",
				Value:  ticketUrl,
				Inline: false,
			},
		},
	}

	var transcriptData = types.FileTranscriptData{
		Issue:         issue,
		TopicID:       topicId,
		Topic:         topic,
		TicketContext: ticketContext,
		Messages:      messages,
		UserID:        userId,
		CloseUserID:   i.Member.User.ID,
		ChannelID:     ticketsChannelId,
		TicketID:      tikId,
	}

	transcript, err := json.Marshal(transcriptData)

	if err != nil {
		logger.Error("Error marshalling transcript", zap.Error(err), zap.String("ticket_id", tikId))

		// Send a message to the user
		_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't create transcript)! Please try again later."),
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		})
		return err
	}

	file := &discordgo.File{
		Name:        tikId + ".ibltranscript",
		ContentType: "application/json+ibltranscript",
		Reader:      bytes.NewReader([]byte(transcript)),
	}

	_, err = s.ChannelMessageSendComplex(config.Channels.LogChannel, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  []*discordgo.File{file},
	})

	if err != nil {
		logger.Error("Error sending transcript to logs channel", zap.Error(err), zap.String("ticket_id", tikId))
		_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("Your ticket couldn't be closed properly (couldn't send transcript)! Please try again later"),
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		})
		return err
	}

	// Create DM if possible
	dm, err := s.UserChannelCreate(userId)

	if err != nil {
		logger.Error("Error creating DM channel", zap.Error(err), zap.String("user_id", userId))
	} else {
		// Reset file reader
		file.Reader = bytes.NewReader([]byte(transcript))

		_, err = s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files:  []*discordgo.File{file},
		})

		if err != nil {
			logger.Error("Error sending transcript to user", zap.Error(err), zap.String("user_id", userId))
		}
	}

	// Set thread to read-only
	var locked = true
	_, err = s.ChannelEdit(ticketsChannelId, &discordgo.ChannelEdit{
		ParentID: os.Getenv("TICKET_THREAD_CHANNEL"),
		Locked:   &locked,
		Archived: &locked,
	})

	if err != nil {
		logger.Error("Error setting thread to read-only", zap.Error(err), zap.String("ticket_id", tikId))
		// Send a message to the user
		_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("Your ticket couldn't be closed properly! Please try again later."),
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		})
		return err
	}

	err = tx.Commit(ctx)

	if err != nil {
		logger.Error("Error committing transaction", zap.Error(err))
		return err
	}

	_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
		Content: utils.Stringp("Your ticket has been closed and can be viewed at: " + ticketUrl),
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	})

	return err
}
