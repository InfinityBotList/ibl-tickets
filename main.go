package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-redis/redis/v8"
	"github.com/infinitybotlist/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
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
}

type Question struct {
	Question    string `yaml:"question"`
	Placeholder string `yaml:"placeholder"`
}

type Message struct {
	ID       string                    `json:"id"`
	Content  string                    `json:"content"`
	Embeds   []*discordgo.MessageEmbed `json:"embeds"`
	AuthorID string                    `json:"author_id"`
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
							},
						})
						return
					}

					// Send the user a message with a link to their open ticket
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "You already have an open ticket. Please use the following link to view it: <#" + ticketsChannelId + ">",
							Flags:   discordgo.MessageFlagsEphemeral,
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

				err = pool.QueryRow(ctx, "SELECT channel_id FROM tickets WHERE id = $1", tikId).Scan(&ticketsChannelId)

				if err != nil {
					fmt.Fprintln(os.Stderr, "Error:", err, ", user ID:", i.Member.User.ID)
					s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Content: "An error occurred while finding this ticket. Please contact our support team about this!",
							Flags:   discordgo.MessageFlagsEphemeral,
						},
					})
					return
				}

				// Send the user a message with a link to their open ticket
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Closing ticket " + tikId + "... Please wait...",
					},
				})

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
						})
						return
					}

					for _, msg := range msgs {
						messages = append(messages, Message{
							ID:       msg.ID,
							AuthorID: msg.Author.ID,
							Content:  msg.Content,
							Embeds:   msg.Embeds,
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
					})
					return
				}

				// Send a message to the user
				newmsg := "Your ticket has been closed and can be viewed at: " + os.Getenv("FRONTEND_URL") + "/transcripts/" + tikId

				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Content: &newmsg,
				})
			}
		case discordgo.InteractionModalSubmit:
			data := i.ModalSubmitData()

			// Create the thread
			/*thread, err := s.ThreadStartComplex(os.Getenv("TICKET_THREAD_CHANNEL=1053875909024817182"), &discordgo.ThreadStart{

			}) */

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
					})
					return
				}

				// Send the answers to the thread
				var answersStr string

				for question, answer := range answers {
					answersStr += "**" + question + "**\n" + answer + "\n\n"
				}

				m, err := s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
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
				})

				if err != nil {
					fmt.Println("Error:", err)
					// Send a message to the user
					newmsg := "Your ticket couldn't be created properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})

					return
				}

				// Pin the message
				err = s.ChannelMessagePin(thread.ID, m.ID)

				if err != nil {
					fmt.Println("Error:", err)
					// Send a message to the user
					newmsg := "Your ticket couldn't be created properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})

					return
				}

				// Add the user to the thread
				err = s.ThreadMemberAdd(thread.ID, i.Member.User.ID)

				if err != nil {
					fmt.Println("Error:", err)
					// Send a message to the user
					newmsg := "Your ticket couldn't be created properly! Please try again later."
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Content: &newmsg,
					})

					return
				}

				// Send a message to the user
				newmsg := "Your ticket has been created! You can view it here: <#" + thread.ID + ">"
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
