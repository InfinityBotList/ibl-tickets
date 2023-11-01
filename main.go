package main

import (
	"context"
	_ "embed"
	"ibl-tickets/handlers/modal"
	"ibl-tickets/handlers/msgcomponent"
	"ibl-tickets/types"
	"ibl-tickets/utils"
	"net/http"
	"os"
	"strings"

	"github.com/bwmarrin/discordgo"
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

func init() {
	iblfile.RegisterFormat(
		"ticket",
		&iblfile.Format{
			Format:  "transcript",
			Version: "a1",
		},
	)
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

			fn, ok := msgcomponent.Handlers[strings.Split(data.CustomID, ":")[0]]

			if !ok {
				logger.Error("Invalid component handler", zap.String("customId", data.CustomID), zap.String("userId", i.Member.User.ID))
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Content: utils.Stringp("An error occurred while handling this component. Please contact our support team about this!"),
				})
				return
			}

			err = fn(s, i.Interaction, data, config, pool, ctx, logger, rediscli)

			if err != nil {
				logger.Error("Error handling component", zap.Error(err), zap.String("customId", data.CustomID), zap.String("userId", i.Member.User.ID))
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Content: utils.Stringp("An error occurred while handling this component. Please contact our support team about this:" + err.Error()),
				})
				return
			}
		case discordgo.InteractionModalSubmit:
			data := i.ModalSubmitData()

			fn, ok := modal.Handlers[strings.Split(data.CustomID, ":")[0]]

			if !ok {
				logger.Error("Invalid modal handler", zap.String("customId", data.CustomID), zap.String("userId", i.Member.User.ID))
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Content: utils.Stringp("An error occurred while handling this modal. Please contact our support team about this!"),
				})
				return
			}

			err = fn(s, i.Interaction, data, config, pool, ctx, logger, rediscli)

			if err != nil {
				logger.Error("Error handling modal", zap.Error(err), zap.String("customId", data.CustomID), zap.String("userId", i.Member.User.ID))
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Content: utils.Stringp("An error occurred while handling this modal. Please contact our support team about this:" + err.Error()),
				})
				return
			}
		}
	})

	err = discord.Open()

	if err != nil {
		panic(err)
	}

	select {}
}
