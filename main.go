package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"ibl-tickets/pem"
	"ibl-tickets/types"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/infinitybotlist/eureka/crypto"
	"github.com/infinitybotlist/eureka/proxy"
	"github.com/infinitybotlist/eureka/snippets"
	"github.com/infinitybotlist/iblfile"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var (
	config *types.Config

	secrets *types.Secrets

	discord *discordgo.Session

	owners Owners

	pool *pgxpool.Pool

	rediscli *redis.Client

	ctx = context.Background()

	logger *zap.Logger
)

type Owners struct {
	Owners []*discordgo.TeamMember
}

func (o Owners) Slice() []string {
	var owners []string

	for _, owner := range o.Owners {
		owners = append(owners, owner.User.Username+"#"+owner.User.Discriminator+" ("+owner.User.ID+")")
	}

	return owners
}

func (o Owners) String() string {
	return strings.Join(o.Slice(), ", ")
}

func (o Owners) IsOwner(userID string) bool {
	for _, owner := range o.Owners {
		if owner.User.ID == userID {
			return true
		}
	}

	return false
}

func createAttachmentBlob(msg *discordgo.Message) ([]types.Attachment, map[string]*bytes.Buffer, error) {
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

func main() {
	logger = snippets.CreateZap()

	f, err := os.Open("config.yaml")

	if err != nil {
		panic(err)
	}

	err = yaml.NewDecoder(f).Decode(&config)

	if err != nil {
		panic(err)
	}

	f.Close()

	f, err = os.Open("secrets.yaml")

	if err != nil {
		panic(err)
	}

	err = yaml.NewDecoder(f).Decode(&secrets)

	if err != nil {
		panic(err)
	}

	f.Close()

	pool, err = pgxpool.New(ctx, config.Database.Postgres)

	if err != nil {
		panic(err)
	}

	rOptions, err := redis.ParseURL(config.Database.Redis)

	if err != nil {
		panic(err)
	}

	rediscli = redis.NewClient(rOptions)

	discord, err = discordgo.New("Bot " + secrets.Token)

	if err != nil {
		panic(err)
	}

	discord.Client.Transport = proxy.NewHostRewriter("localhost:3219", http.DefaultTransport, func(s string) {
		logger.Info("[PROXY]", zap.String("note", s))
	})

	// Get bot owners using the Discord API, @me is used here to get the bot's application
	app, err := discord.Application("@me")

	if err != nil {
		panic(err)
	}

	if app.Team == nil {
		logger.Error("Bot is not in a team, please add it to a team to use this bot.")
		os.Exit(1)
	}

	owners = Owners{Owners: app.Team.Members}

	logger.Error("Bot owners", zap.Strings("owners", owners.Slice()))

	discord.Identify.Intents = discordgo.IntentsAllWithoutPrivileged | discordgo.IntentsMessageContent | discordgo.IntentsGuildMembers

	discord.AddHandler(func(s *discordgo.Session, i *discordgo.Ready) {
		logger.Info("Bot is ready", zap.String("username", i.User.Username+"#"+i.User.Discriminator), zap.String("userId", i.User.ID))
	})

	discord.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		// Check that the message starts with mentioning the bot
		var mentioned bool
		for _, user := range m.Mentions {
			if user.ID == discord.State.User.ID {
				mentioned = true
				break
			}
		}

		if mentioned {
			m.Content = strings.TrimSpace(strings.TrimPrefix(m.Content, "<@"+discord.State.User.ID+">"))
			m.Content = strings.TrimSpace(strings.TrimPrefix(m.Content, "<@!"+discord.State.User.ID+">"))

			if !owners.IsOwner(m.Author.ID) {
				_, err := s.ChannelMessageSend(m.ChannelID, "You are not allowed to use this bot.")

				if err != nil {
					logger.Error("Error sending message", zap.Error(err))
					return
				}
			}

			switch m.Content {
			case "msg":
				// Delete all messages in the channel
				messages, err := s.ChannelMessages(m.ChannelID, 100, "", "", "")

				if err != nil {
					panic(err)
				}

				for _, message := range messages {
					err = s.ChannelMessageDelete(m.ChannelID, message.ID)

					if err != nil {
						panic(err)
					}
				}

				// Send the ticket message
				var smo []discordgo.SelectMenuOption

				for key, topic := range config.Topics {
					smo = append(smo, discordgo.SelectMenuOption{
						Label:       topic.Name,
						Value:       key,
						Description: topic.Description,
						Emoji: discordgo.ComponentEmoji{
							Name: topic.Emoji,
						},
					})
				}

				_, err = s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
					Embeds: []*discordgo.MessageEmbed{
						{
							Title:       "How can we help?",
							Type:        discordgo.EmbedTypeRich,
							Description: "Please select a topic below to create a ticket. If you don't see a topic that fits your issue, please create a ticket with the `General Support` topic.",
						},
					},
					Components: []discordgo.MessageComponent{
						discordgo.ActionsRow{
							Components: []discordgo.MessageComponent{
								&discordgo.SelectMenu{
									CustomID:    "tikm",
									Placeholder: "How can we help you",
									Options:     smo,
								},
							},
						},
					},
				})

				if err != nil {
					_, merr := s.ChannelMessageSend(m.ChannelID, "An error occurred while sending the message:"+err.Error())

					if err != nil {
						logger.Error("Error sending message", zap.Error(err), zap.NamedError("merr", merr), zap.String("channelId", m.ChannelID), zap.String("userId", m.Author.ID))
					}
				}
			}
		}
	})

	discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionMessageComponent:
			data := i.MessageComponentData()

			switch strings.Split(data.CustomID, ":")[0] {
			case "tikm":
				// Edit existing message to reset the select menu
				_, err = s.ChannelMessageEditComplex(&discordgo.MessageEdit{
					Embeds:     i.Message.Embeds,
					Components: i.Message.Components,
					ID:         i.Message.ID,
					Channel:    i.Message.ChannelID,
				})

				if err != nil {
					logger.Error("Error resetting select menu", zap.Error(err), zap.String("channelId", i.Message.ChannelID), zap.String("userId", i.Member.User.ID), zap.String("customId", data.CustomID))
				}

				topicId := data.Values[0]
				logger.Info("Creating ticket", zap.String("topicId", topicId), zap.String("userId", i.Member.User.ID))

				// Create new ticket under ticket channel via private threads
				topic, ok := config.Topics[topicId]

				if !ok {
					logger.Error("Invalid topic ID", zap.String("topicId", topicId), zap.String("userId", i.Member.User.ID))
					return
				}

				// Check cooldown from redis
				cooldownKey := "ticket_cooldown:" + i.Member.User.ID

				cooldown := rediscli.TTL(ctx, cooldownKey).Val()

				if cooldown == -2 || cooldown == -1 {
					// Set cooldown
					err = rediscli.Set(ctx, cooldownKey, "0", 10*time.Second).Err()

					if err != nil {
						logger.Error("Error setting cooldown", zap.Error(err), zap.String("userId", i.Member.User.ID))
						return
					}
				} else {
					// Cooldown exists
					logger.Info("User is on cooldown", zap.String("userId", i.Member.User.ID), zap.Duration("cooldown", cooldown))

					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "You are on cooldown. Please wait " + cooldown.String() + " before creating another ticket.",
							Flags:   discordgo.MessageFlagsEphemeral,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
				}

				modalqas := make([]discordgo.MessageComponent, len(topic.Questions)+1)

				modalqas[0] = discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						&discordgo.TextInput{
							Label:       "Topic?",
							Placeholder: "What is your issue? ",
							MinLength:   1,
							MaxLength:   1000,
							CustomID:    "issue",
							Required:    true,
							Style:       discordgo.TextInputShort,
						},
					},
				}

				for i, question := range topic.Questions {
					modalqas[i+1] = discordgo.ActionsRow{
						Components: []discordgo.MessageComponent{
							&discordgo.TextInput{
								Label:       question.Question,
								Placeholder: question.Placeholder,
								MinLength:   1,
								MaxLength:   4000,
								CustomID:    strconv.Itoa(i),
								Required:    true,
								Style:       discordgo.TextInputShort,
							},
						},
					}
				}

				err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseModal,
					Data: &discordgo.InteractionResponseData{
						CustomID:   "tikmodal:" + topicId,
						Title:      topic.Name,
						Components: modalqas,
					},
				})

				if err != nil {
					logger.Error("Error sending message", zap.Error(err), zap.String("userId", i.Member.User.ID))
				}
			case "close":
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
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while finding this ticket. Please contact our support team about this!",
							Flags:   discordgo.MessageFlagsEphemeral,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				defer tx.Rollback(ctx)

				err = tx.QueryRow(ctx, "SELECT issue, topic_id, user_id, channel_id, ticket_context, open FROM tickets WHERE id = $1", tikId).Scan(&issue, &topicId, &userId, &ticketsChannelId, &ticketContext, &open)

				if err != nil {
					logger.Error("Error getting ticket from database", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while finding this ticket. Please contact our support team about this!",
							Flags:   discordgo.MessageFlagsEphemeral,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				if !open {
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "This ticket is already closed?!",
							Flags:   discordgo.MessageFlagsEphemeral,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				if ticketsChannelId != i.ChannelID {
					err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "You can't close a ticket that isn't in this channel!",
							Flags:   discordgo.MessageFlagsEphemeral,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})

					if err != nil {
						logger.Error("Error sending message", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
					}
					return
				}

				// Respond with interrim closing ticket message/warning
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
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
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while generating RSA encryption keys for this ticket. Please contact our support team about this!",
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
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

								attachments, cdnBlob, err = createAttachmentBlob(msg)

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
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while getting messages for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
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
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while creating a transcript for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				iblf := iblfile.New()

				for key, value := range encMap {
					err = iblf.WriteSection(value, key)

					if err != nil {
						logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: "An error occurred while creating a transcript for this ticket. Please contact our support team about this:" + err.Error(),
								AllowedMentions: &discordgo.MessageAllowedMentions{
									Parse: []discordgo.AllowedMentionType{},
								},
							},
						})
						return
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
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while creating a transcript for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				metadata.FormatVersion = f.Version

				mdb, err := json.Marshal(metadata)

				if err != nil {
					logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while creating a transcript for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				err = iblf.WriteSection(bytes.NewBuffer(mdb), "metadata")

				if err != nil {
					logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while creating a transcript for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				var transcriptOutput *bytes.Buffer

				err = iblf.WriteOutput(transcriptOutput)

				if err != nil {
					logger.Error("Error creating transcript", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while creating a transcript for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				// Save transcript file to cdn
				file, err := os.Create(config.Database.FileStoragePath + "/" + tikId + ".ibltranscript")

				if err != nil {
					logger.Error("Error saving transcript file", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while saving a transcript for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				_, err = io.Copy(file, transcriptOutput)

				if err != nil {
					logger.Error("Error saving transcript file", zap.Error(err), zap.String("ticketId", tikId), zap.String("userId", i.Member.User.ID))
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while saving a transcript for this ticket. Please contact our support team about this:" + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
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
					newmsg := "Your ticket couldn't be closed properly (couldn't send transcript)! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
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

				newmsg := "Your ticket has been closed. A ibltranscript of this ticket can be found at: " + config.Database.ExposedPath + "/" + tikId + ".ibltranscript"
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Content: &newmsg,
					AllowedMentions: &discordgo.MessageAllowedMentions{
						Parse: []discordgo.AllowedMentionType{},
					},
				})
			}
		case discordgo.InteractionModalSubmit:
			data := i.ModalSubmitData()

			switch strings.Split(data.CustomID, ":")[0] {
			case "tikmodal":
				topicId := strings.Split(data.CustomID, ":")[1]

				topic, ok := config.Topics[topicId]

				if !ok {
					// Send a message to the user
					logger.Error("Invalid topic ID", zap.String("topicId", topicId))
					newmsg := "Your tickets topic is invalid (somehow)! Please contact support or try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				// Send a message to the user
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Creating ticket.\n\nPlease wait...",
						Flags:   discordgo.MessageFlagsEphemeral,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					},
				})

				var answers = map[string]string{}
				var issue string

				for _, value := range data.Components {
					// Get the question
					input := value.(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput)

					if input.CustomID == "issue" {
						issue = input.Value
						continue
					}

					questionNum, err := strconv.Atoi(input.CustomID)

					if err != nil {
						logger.Error("Error converting question number to int", zap.Error(err), zap.String("customId", input.CustomID))
						// Send a message to the user
						newmsg := "Your ticket is invalid! Please try again later."
						s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
							Content: &newmsg,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						})

						return
					}

					answers[topic.Questions[questionNum].Question] = input.Value
				}

				thread, err := s.ThreadStartComplex(config.Channels.ThreadChannel, &discordgo.ThreadStart{
					Name: issue,
					Type: discordgo.ChannelTypeGuildPrivateThread,
				})

				if err != nil {
					// Send a message to the user
					logger.Error("Error creating thread", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId))
					newmsg := "Your ticket couldn't be created properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})

					return
				}

				tikId := crypto.RandString(64)

				// Add the ticket to the database
				_, err = pool.Exec(ctx, "INSERT INTO tickets (id, user_id, channel_id, topic_id, ticket_context, issue) VALUES ($1, $2, $3, $4, $5, $6)", tikId, i.Member.User.ID, thread.ID, topicId, answers, issue)

				if err != nil {
					logger.Error("Error inserting ticket into database", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId))
					// Send a message to the user
					newmsg := "Your ticket couldn't be created properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				// Send the answers to the thread
				var answersStr string

				for question, answer := range answers {
					answersStr += "**" + question + "**\n" + answer + "\n\n"
				}

				rolesToPing := topic.Ping

				var rolesStr string

				for _, role := range rolesToPing {
					rolesStr += "<@&" + role + "> "
				}

				m, err := s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
					Content: i.Member.User.Mention() + " " + rolesStr,
					Embeds: []*discordgo.MessageEmbed{
						{
							Title:       "Ticket created by " + i.Member.User.Username + "#" + i.Member.User.Discriminator,
							Description: answersStr,
							Fields: []*discordgo.MessageEmbedField{
								{
									Name:   "Issue",
									Value:  issue,
									Inline: false,
								},
								{
									Name:   "Ticket ID",
									Value:  tikId,
									Inline: false,
								},
								{
									Name:  "Topic ID",
									Value: topicId,
								},
							},
						},
					},
					Components: []discordgo.MessageComponent{
						discordgo.ActionsRow{
							Components: []discordgo.MessageComponent{
								discordgo.Button{
									Label:    "Close",
									Style:    discordgo.SuccessButton,
									CustomID: "close:" + tikId,
								},
							},
						},
					},
					AllowedMentions: &discordgo.MessageAllowedMentions{
						Roles: rolesToPing,
					},
				})

				if err != nil {
					logger.Error("Error sending message", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId))
					// Send a message to the user
					newmsg := "Your ticket couldn't be created properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})
					return
				}

				deleteThread := func() error {
					_, err := pool.Exec(ctx, "DELETE FROM tickets WHERE id = $1", tikId)

					if err != nil {
						return fmt.Errorf("error deleting ticket from database: %w", err)
					}

					_, err = s.ChannelDelete(thread.ID)

					if err != nil {
						return fmt.Errorf("error deleting thread: %w", err)
					}

					return nil
				}

				err = s.ThreadMemberAdd(thread.ID, i.Member.User.ID)

				if err != nil {
					logger.Error("Error adding user to thread", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId), zap.String("userId", i.Member.User.ID))
					// Send a message to the user
					newmsg := "You couldn't be added to the ticket! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})

					err = deleteThread()

					if err != nil {
						logger.Error("Error deleting thread", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId), zap.String("userId", i.Member.User.ID))
					}
					return
				}

				// Pin the message
				err = s.ChannelMessagePin(thread.ID, m.ID)

				if err != nil {
					logger.Error("Error pinning message", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId))
					// Send a message to the user
					newmsg := "Your ticket couldn't be pinned properly but it appears to have been created! You can view it here: (https://discord.com/channels/" + i.Interaction.GuildID + "/" + thread.ID + ")"
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})
					return
				}

				// Send a message to the user
				newmsg := "Your ticket has been created! You can view it here: (https://discord.com/channels/" + i.Interaction.GuildID + "/" + thread.ID + ")"
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Content: &newmsg,
				})
			}
		}
	})

	err = discord.Open()

	if err != nil {
		panic(err)
	}

	select {}
}
