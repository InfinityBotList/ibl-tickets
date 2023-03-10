package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-redis/redis/v8"
	"github.com/infinitybotlist/eureka/crypto"
	"github.com/infinitybotlist/eureka/proxy"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"golang.org/x/exp/slices"
	"gopkg.in/yaml.v3"
)

var (
	//go:embed topics.yaml
	topicsBytes []byte

	topics map[string]Topic

	discord *discordgo.Session

	owners Owners

	pool *pgxpool.Pool

	rediscli *redis.Client

	ctx = context.Background()
)

type Owners struct {
	Owners []*discordgo.TeamMember
}

func (o Owners) String() string {
	var owners []string

	for _, owner := range o.Owners {
		owners = append(owners, owner.User.Username+"#"+owner.User.Discriminator+" ("+owner.User.ID+")")
	}

	return strings.Join(owners, ", ")
}

func (o Owners) IsOwner(userID string) bool {
	for _, owner := range o.Owners {
		if owner.User.ID == userID {
			return true
		}
	}

	return false
}

type Topic struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Emoji       string     `yaml:"emoji"`
	Questions   []Question `yaml:"questions"`
	PingExtra   []string   `yaml:"pingExtra"`
}

type Question struct {
	Question    string `yaml:"question"`
	Placeholder string `yaml:"placeholder"`
}

type Message struct {
	ID          string                         `json:"id"`
	Content     string                         `json:"content"`
	Embeds      []*discordgo.MessageEmbed      `json:"embeds"`
	AuthorID    string                         `json:"author_id"`
	Attachments []*discordgo.MessageAttachment `json:"attachments"`
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
	TicketURL     string            `json:"ticket_url"`
}

func main() {
	godotenv.Load()

	var connUrl string
	var redisUrl string

	flag.StringVar(&connUrl, "db", "postgresql:///infinity", "Database connection URL")
	flag.StringVar(&redisUrl, "redis", "redis://localhost:6379", "Redis connection URL")
	flag.Parse()

	var err error
	pool, err = pgxpool.New(ctx, connUrl)

	if err != nil {
		panic(err)
	}

	rOptions, err := redis.ParseURL(redisUrl)

	if err != nil {
		panic(err)
	}

	rediscli = redis.NewClient(rOptions)

	err = yaml.Unmarshal(topicsBytes, &topics)

	if err != nil {
		panic(err)
	}

	discord, err = discordgo.New("Bot " + os.Getenv("DISCORD_TOKEN"))

	if err != nil {
		panic(err)
	}

	discord.Client.Transport = proxy.NewHostRewriter("localhost:3219", http.DefaultTransport, func(s string) {
		fmt.Println("[PROXY]", s)
	})

	// Get bot owners using the Discord API, @me is used here to get the bot's application
	app, err := discord.Application("@me")

	if err != nil {
		panic(err)
	}

	if app.Team == nil {
		fmt.Fprintln(os.Stderr, "Bot is not in a team, please add it to a team to use this bot.")
		os.Exit(1)
	}

	owners = Owners{Owners: app.Team.Members}

	fmt.Println("Bot owners:", owners.String())

	discord.Identify.Intents = discordgo.IntentsAllWithoutPrivileged | discordgo.IntentsMessageContent | discordgo.IntentsGuildMembers

	discord.AddHandler(func(s *discordgo.Session, i *discordgo.Ready) {
		fmt.Println("Bot is ready. Logged in as " + i.User.Username + "#" + i.User.Discriminator)
	})

	discord.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if owners.IsOwner(m.Author.ID) {
			if m.Content == "tikm" && m.ChannelID == os.Getenv("TICKET_CREATE_CHANNEL") {
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

				for key, topic := range topics {
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
					fmt.Println("Error:", err)
				}
			}

			if m.Content == "cbt" && m.ChannelID == os.Getenv("TICKET_THREAD_CHANNEL") {
				s.ChannelMessageDelete(m.ChannelID, m.ID)

				// Get all threads, regardless of archived or not
				var activeThreads []string
				var archivedThreads []string
				var privateArchivedThreads []string

				// Active threads
				threads, err := s.GuildThreadsActive(m.GuildID)

				if err != nil {
					fmt.Println("Error:", err)
					s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
						Content: "Error: " + err.Error(),
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				for _, thread := range threads.Threads {
					if thread.ParentID == m.ChannelID {
						activeThreads = append(activeThreads, thread.ID)
					}
				}

				// Archived threads
				for {
					threads, err := s.ThreadsArchived(m.ChannelID, nil, 0)

					if err != nil {
						fmt.Println("Error:", err)
						s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
							Content: "Error: " + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						})
						return
					}

					for _, thread := range threads.Threads {
						archivedThreads = append(archivedThreads, thread.ID)
					}

					if !threads.HasMore {
						break
					}
				}

				// Private archived threads
				for {
					threads, err := s.ThreadsPrivateArchived(m.ChannelID, nil, 0)

					if err != nil {
						fmt.Println("Error:", err)
						s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
							Content: "Error: " + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						})
						return
					}

					for _, thread := range threads.Threads {
						privateArchivedThreads = append(privateArchivedThreads, thread.ID)
					}

					if !threads.HasMore {
						break
					}
				}

				fmt.Println("Active threads:", activeThreads)
				fmt.Println("Archived threads:", archivedThreads)
				fmt.Println("Private archived threads:", privateArchivedThreads)

				// Combine all threads
				allThreads := append(activeThreads, archivedThreads...)
				allThreads = append(allThreads, privateArchivedThreads...)

				// Get all tickets
				rows, err := pool.Query(ctx, "SELECT id, channel_id FROM tickets")

				if err != nil {
					fmt.Println("Error:", err)
					s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
						Content: "Error: " + err.Error(),
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				defer rows.Close()

				for rows.Next() {
					var ticketId string
					var channelId string

					err = rows.Scan(&ticketId, &channelId)

					if err != nil {
						fmt.Println("Error:", err)
						s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
							Content: "Error: " + err.Error(),
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						})
						return
					}

					// Check if ticket exists in threads
					if !slices.Contains(allThreads, channelId) {
						// Close ticket
						_, err = pool.Exec(ctx, "UPDATE tickets SET open = false WHERE id = $1", ticketId)

						if err != nil {
							fmt.Println("Error:", err)
							s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
								Content: "Error: " + err.Error(),
								AllowedMentions: &discordgo.MessageAllowedMentions{
									Parse: []discordgo.AllowedMentionType{},
								},
							})
							return
						}

						s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
							Content: "Closed ticket " + ticketId,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						})
					}
				}
			}
		}
	})

	discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		fmt.Println("Interaction:", i.Data)

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
					fmt.Println("Error:", err)
				}

				topicId := data.Values[0]
				fmt.Println("TicketCreate:", topicId)

				// Create new ticket under ticket channel via private threads
				topic, ok := topics[topicId]

				if !ok {
					fmt.Println("Invalid topic ID:", topicId)
					return
				}

				// Check cooldown from redis
				cooldownKey := "ticket_cooldown:" + i.Member.User.ID

				cooldown := rediscli.TTL(ctx, cooldownKey).Val()

				if cooldown == -2 || cooldown == -1 {
					// Set cooldown
					err = rediscli.Set(ctx, cooldownKey, "0", 10*time.Second).Err()

					if err != nil {
						fmt.Println("Error:", err)
						return
					}
				} else {
					// Cooldown exists
					fmt.Println("Cooldown active for user:", i.Member.User.ID)

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

				// Ensure that the user does not already have a open ticket
				var count int64
				err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM tickets WHERE user_id = $1 AND open = true", i.Member.User.ID).Scan(&count)

				if err != nil {
					fmt.Fprintln(os.Stderr, "Error:", err, ", user ID:", i.Member.User.ID)
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while creating your ticket. Please try again later.",
							Flags:   discordgo.MessageFlagsEphemeral,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				if count > 0 {
					// Get the open tickets channel ID
					var ticketsChannelId string

					err = pool.QueryRow(ctx, "SELECT channel_id FROM tickets WHERE user_id = $1 AND open = true", i.Member.User.ID).Scan(&ticketsChannelId)

					if err != nil {
						fmt.Fprintln(os.Stderr, "Error:", err, ", user ID:", i.Member.User.ID)
						s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseChannelMessageWithSource,
							Data: &discordgo.InteractionResponseData{
								Content: "An error occurred while finding your last open ticket. Please contact our support team about this!",
								Flags:   discordgo.MessageFlagsEphemeral,
								AllowedMentions: &discordgo.MessageAllowedMentions{
									Parse: []discordgo.AllowedMentionType{},
								},
							},
						})
						return
					}

					// Send the user a message with a link to their open ticket
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "You already have an open ticket. Please use the following link to view it: <#" + ticketsChannelId + "> (https://discord.com/channels/" + i.Interaction.GuildID + "/" + ticketsChannelId + ")",
							Flags:   discordgo.MessageFlagsEphemeral,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						},
					})
					return
				}

				modalqas := make([]discordgo.MessageComponent, 1+len(topic.Questions))

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
					fmt.Println("Error:", err)
				}
			case "close":
				tikId := strings.Split(data.CustomID, ":")[1]

				// Get the open tickets channel ID
				var ticketsChannelId string
				var open bool
				var userId string
				var issue string
				var topicId string
				var ticketContext map[string]string

				err = pool.QueryRow(ctx, "SELECT issue, topic_id, user_id, channel_id, open, ticket_context FROM tickets WHERE id = $1", tikId).Scan(&issue, &topicId, &userId, &ticketsChannelId, &open, &ticketContext)

				if err != nil {
					fmt.Fprintln(os.Stderr, "Error:", err, ", user ID:", i.Member.User.ID)
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

				if ticketsChannelId != i.ChannelID {
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "You can't close a ticket that isn't in this channel!",
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

				// Send the user a message with a link to their open ticket
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Closing ticket " + tikId + "... Please wait...",
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					},
				})

				// Update the database setting open to false
				_, err = pool.Exec(ctx, "UPDATE tickets SET open = false, close_user_id = $2 WHERE id = $1", tikId, i.Member.User.ID)

				if err != nil {
					fmt.Fprintln(os.Stderr, "Error:", err, ", user ID:", i.Member.User.ID)
					var content = "An error occurred while closing this ticket. Please contact our support team about this!"
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &content,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				// Set thread to read-only
				var locked = true
				_, err = s.ChannelEdit(ticketsChannelId, &discordgo.ChannelEdit{
					ParentID: os.Getenv("TICKET_THREAD_CHANNEL"),
					Locked:   &locked,
					Archived: &locked,
				})

				if err != nil {
					fmt.Println("Error:", err)
					// Send a message to the user
					newmsg := "Your ticket couldn't be closed properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				// Collect every message in the channel
				var messages []Message

				var lastMessageId string
				for {
					msgs, err := s.ChannelMessages(ticketsChannelId, 100, lastMessageId, "", "")

					if err != nil {
						fmt.Println("Error:", err)
						// Send a message to the user
						newmsg := "Your ticket couldn't be closed properly (couldn't find messages)! Please try again later.\nlastMessageId=" + lastMessageId
						s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
							Content: &newmsg,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								Parse: []discordgo.AllowedMentionType{},
							},
						})
						return
					}

					for _, msg := range msgs {
						messages = append(messages, Message{
							ID:          msg.ID,
							AuthorID:    msg.Author.ID,
							Content:     msg.Content,
							Embeds:      msg.Embeds,
							Attachments: msg.Attachments,
						})
					}

					if len(msgs) < 100 {
						break
					}

					lastMessageId = msgs[len(msgs)-1].ID
				}

				// Update database with the messages
				_, err := pool.Exec(ctx, "UPDATE tickets SET messages = $1 WHERE id = $2", messages, tikId)

				if err != nil {
					fmt.Println("Error:", err)
					// Send a message to the user
					newmsg := "Your ticket couldn't be closed properly (couldn't update database)! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				ticketUrl := os.Getenv("FRONTEND_URL") + "/ticket/" + tikId

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

				var transcriptData = FileTranscriptData{
					Issue:         issue,
					TopicID:       topicId,
					Topic:         topics[topicId],
					TicketContext: ticketContext,
					Messages:      messages,
					UserID:        userId,
					CloseUserID:   i.Member.User.ID,
					ChannelID:     ticketsChannelId,
					TicketID:      tikId,
					TicketURL:     ticketUrl,
				}

				transcript, err := json.Marshal(transcriptData)

				if err != nil {
					fmt.Println("Error:", err)
					// Send a message to the user
					newmsg := "Your ticket couldn't be closed properly (couldn't create transcript)! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				file := &discordgo.File{
					Name:        tikId + ".ibltranscript",
					ContentType: "application/json+ibltranscript",
					Reader:      bytes.NewReader([]byte(transcript)),
				}

				_, err = s.ChannelMessageSendComplex(os.Getenv("TICKET_LOGS_CHANNEL"), &discordgo.MessageSend{
					Embeds: []*discordgo.MessageEmbed{embed},
					Files:  []*discordgo.File{file},
				})

				if err != nil {
					fmt.Println("Error:", err)
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
					fmt.Println("DM Channel Create Error [ignoring as not critical]:", err)
				} else {
					// Reset file reader
					file.Reader = bytes.NewReader([]byte(transcript))

					_, err = s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
						Embeds: []*discordgo.MessageEmbed{embed},
						Files:  []*discordgo.File{file},
					})

					if err != nil {
						fmt.Println("DM Channel Send Error [ignoring as not critical]:", err)
					}
				}

				newmsg := "Your ticket has been closed and can be viewed at: " + ticketUrl
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

				topic, ok := topics[topicId]

				if !ok {
					fmt.Println("Invalid topic ID:", topicId)
					// Send a message to the user
					newmsg := "Your tickets topic is invalid! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
						AllowedMentions: &discordgo.MessageAllowedMentions{
							Parse: []discordgo.AllowedMentionType{},
						},
					})
					return
				}

				fmt.Println("TicketModalSubmit:", data)

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
						fmt.Println("Error:", err)
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

				fmt.Println("Answers:", answers)

				thread, err := s.ThreadStartComplex(os.Getenv("TICKET_THREAD_CHANNEL"), &discordgo.ThreadStart{
					Name: issue,
					Type: discordgo.ChannelTypeGuildPrivateThread,
				})

				if err != nil {
					fmt.Println("Error:", err)
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

				tikId := crypto.RandString(64)

				// Add the ticket to the database
				_, err = pool.Exec(ctx, "INSERT INTO tickets (id, user_id, channel_id, topic_id, ticket_context, issue) VALUES ($1, $2, $3, $4, $5, $6)", tikId, i.Member.User.ID, thread.ID, topicId, answers, issue)

				if err != nil {
					fmt.Println(err)
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

				rolesToPing := []string{os.Getenv("SUPPORT_ROLE")}

				rolesToPing = append(rolesToPing, topic.PingExtra...)

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
					fmt.Println("Error:", err)
					// Send a message to the user
					newmsg := "Your ticket couldn't be created properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})
					pool.Exec(ctx, "DELETE FROM tickets WHERE id = $1", tikId)
					return
				}

				err = s.ThreadMemberAdd(thread.ID, i.Member.User.ID)

				if err != nil {
					fmt.Println("ErrorTadd:", err)
					// Send a message to the user
					newmsg := "You couldn't be added to the ticket! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})
					pool.Exec(ctx, "DELETE FROM tickets WHERE id = $1", tikId)
					s.ChannelDelete(thread.ID)
					return
				}

				// Pin the message
				err = s.ChannelMessagePin(thread.ID, m.ID)

				if err != nil {
					fmt.Println("Error:", err)
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
