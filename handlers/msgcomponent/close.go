package msgcomponent

import (
	"bytes"
	"context"
	"fmt"
	"ibl-tickets/types"
	"ibl-tickets/utils"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/infinitybotlist/eureka/pem"
	"github.com/infinitybotlist/iblfile"
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
	}

	return attachments, bufs, nil
}

func close(s *discordgo.Session, i *discordgo.Interaction, data discordgo.MessageComponentInteractionData, config *types.Config, pool *pgxpool.Pool, ctx context.Context, logger *zap.Logger, rediscli *redis.Client) error {
	// Respond with interrim closing ticket message/warning
	tikId := strings.Split(data.CustomID, ":")[1]

	// Get the open tickets channel ID
	var ticketsChannelId string
	var userId string
	var issue string
	var topicId string
	var ticketContext map[string]string
	var open bool

	tx, err := pool.Begin(ctx)

	if err != nil {
		logger.Error("Error beginning transaction", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))

		// Make sure theres a response to edit
		s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Please wait...",
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})

		return fmt.Errorf("error beginning transaction: %w", err)
	}

	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, "SELECT issue, topic_id, user_id, channel_id, ticket_context, open FROM tickets WHERE id = $1", tikId).Scan(&issue, &topicId, &userId, &ticketsChannelId, &ticketContext, &open)

	if err != nil {
		logger.Error("Error getting ticket from database", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))

		// Make sure theres a response to edit
		s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Please wait...",
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})

		return fmt.Errorf("error getting ticket from database: %w", err)
	}

	if !open {
		s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This ticket is already closed?!",
				Flags:   discordgo.MessageFlagsEphemeral,
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})
		return nil
	}

	if ticketsChannelId != i.ChannelID {
		err = s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "You can't close a ticket from outside its thread!",
				Flags:   discordgo.MessageFlagsEphemeral,
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})

		if err != nil {
			logger.Error("Error sending message", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		}
		return nil
	}

	// Respond with interrim closing ticket message/warning
	s.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Closing ticket " + tikId + "... Please wait...",
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		},
	})

	// Create PEM key secret
	priv, pub, err := pem.MakePem()

	if err != nil {
		logger.Error("Error creating PEM key", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("An error occurred while generating RSA encryption keys for this ticket. Please contact our support team about this!"),
		})
		return fmt.Errorf("error creating PEM key: %w", err)
	}

	var initialSections = map[string]*bytes.Buffer{}

	getMessages := func() (*bytes.Buffer, error) {
		// Collect every message in the channel
		var messages []types.Message
		var lastMessageId string
		for {
			msgs, err := s.ChannelMessages(ticketsChannelId, 100, lastMessageId, "", "")

			if err != nil {
				return nil, fmt.Errorf("error getting messages: %w", err)
			}

			for _, msg := range msgs {
				var attachments []types.Attachment

				if len(msg.Attachments) > 0 {
					var cdnBlob map[string]*bytes.Buffer

					attachments, cdnBlob, err = _createAttachmentBlob(logger, msg)

					if err != nil {
						return nil, fmt.Errorf("error creating attachment blob: %w", err)
					}

					for id, buf := range cdnBlob {
						initialSections["attachments/"+id] = buf
					}
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

		bt, err := json.Marshal(messages)

		if err != nil {
			return nil, fmt.Errorf("error marshalling messages: %w", err)
		}

		return bytes.NewBuffer(bt), nil
	}

	buf, err := getMessages()

	if err != nil {
		logger.Error("Error getting messages", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error getting messages: %w", err)
	}

	var sectionsToEnc = []iblfile.DataEncrypt{
		{
			Section: "data",
			Pubkey:  pub,
			Data: func() (*bytes.Buffer, error) {
				return buf, nil
			},
		},
		{
			Section: "transcriptMeta",
			Pubkey:  pub,
			Data: func() (*bytes.Buffer, error) {
				transcript := types.FileTranscriptData{
					Issue:         issue,
					TopicID:       topicId,
					Topic:         config.Topics[topicId],
					TicketContext: ticketContext,
					UserID:        userId,
					CloseUserID:   i.Member.User.ID,
					ChannelID:     ticketsChannelId,
					TicketID:      tikId,
				}

				bt, err := json.Marshal(transcript)

				if err != nil {
					return nil, fmt.Errorf("error marshalling transcript meta: %w", err)
				}

				return bytes.NewBuffer(bt), nil
			},
		},
	}

	for key, value := range initialSections {
		sectionsToEnc = append(sectionsToEnc, iblfile.DataEncrypt{
			Section: key,
			Pubkey:  pub,
			Data: func() (*bytes.Buffer, error) {
				return value, nil
			},
		})
	}

	encMap, encDataMap, err := iblfile.EncryptSections(
		sectionsToEnc...,
	)

	if err != nil {
		logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error encrypting sections: %w", err)
	}

	iblf := iblfile.New()

	for key, value := range encMap {
		err = iblf.WriteSection(value, key)

		if err != nil {
			logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
			return fmt.Errorf("error writing section: %w", err)
		}
	}

	fileType := "ticket.transcript"
	metadata := iblfile.Meta{
		CreatedAt:      time.Now(),
		Protocol:       iblfile.Protocol,
		Type:           fileType,
		EncryptionData: encDataMap,
	}

	f, err := iblfile.GetFormat(fileType)

	if err != nil {
		logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error getting format: %w", err)
	}

	metadata.FormatVersion = f.Version

	mdb, err := json.Marshal(metadata)

	if err != nil {
		logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error marshalling metadata: %w", err)
	}

	err = iblf.WriteSection(bytes.NewBuffer(mdb), "meta")

	if err != nil {
		logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error writing metadata: %w", err)
	}

	var transcriptOutput = bytes.NewBuffer([]byte{})

	err = iblf.WriteOutput(transcriptOutput)

	if err != nil {
		logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error writing output: %w", err)
	}

	// Save transcript file to cdn
	file, err := os.Create(config.Database.FileStoragePath + "/" + tikId + ".ibltranscript")

	if err != nil {
		logger.Error("Error saving transcript file", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error creating transcript file: %w", err)
	}

	_, err = io.Copy(file, transcriptOutput)

	if err != nil {
		logger.Error("Error saving transcript file", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error copying transcript file: %w", err)
	}

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
				Value:  config.Database.ExposedPath + "/" + tikId + ".ibltranscript",
				Inline: false,
			},
		},
	}

	files := []*discordgo.File{
		{
			Name:        "enckey.pem",
			Reader:      bytes.NewReader(priv),
			ContentType: "application/x-pem-file",
		},
	}

	_, err = s.ChannelMessageSendComplex(config.Channels.LogChannel, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  files,
	})

	if err != nil {
		logger.Error("Error sending log message", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error sending log message: %w", err)
	}

	// Create DM if possible
	dm, err := s.UserChannelCreate(userId)

	if err != nil {
		logger.Warn("Error creating DM channel", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
	} else {
		files := []*discordgo.File{
			{
				Name:        "enckey.pem",
				Reader:      bytes.NewReader(priv),
				ContentType: "application/x-pem-file",
			},
		}

		_, err = s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed},
			Files:  files,
		})

		if err != nil {
			logger.Error("Error sending DM message", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		}
	}

	_, err = tx.Exec(ctx, "UPDATE tickets SET open = false WHERE id = $1", tikId)

	if err != nil {
		logger.Error("Error updating in transaction", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("An error occurred while updating in a transaction for this ticket. Please contact our support team about this:" + err.Error()),
		})
	}

	err = tx.Commit(ctx)

	if err != nil {
		logger.Error("Error committing transaction", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("An error occurred while committing a transaction for this ticket. Please contact our support team about this:" + err.Error()),
		})
	}

	closedBool := true

	_, err = s.ChannelEditComplex(ticketsChannelId, &discordgo.ChannelEdit{
		ParentID: config.Channels.ThreadChannel,
		Archived: &closedBool,
		Locked:   &closedBool,
	})

	if err != nil {
		logger.Error("Error saving thread state", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
		s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content: utils.Stringp("An error occurred while saving thread state for this ticket. Please contact our support team about this:" + err.Error()),
		})
	}

	newmsg := "Your ticket has been closed. A ibltranscript of this ticket can be found at: " + config.Database.ExposedPath + "/" + tikId + ".ibltranscript"
	s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
		Content: &newmsg,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	})

	return nil
}
