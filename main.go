package main

import (
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

var (
	//go:embed topics.yaml
	topicsBytes []byte

	topics map[string]Topic

	discord *discordgo.Session

	owners Owners
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

func main() {
	godotenv.Load()

	err := yaml.Unmarshal(topicsBytes, &topics)

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

	owners = Owners{Owners: app.Team.Members}

	fmt.Println("Bot owners:", owners.String())

	discord.Identify.Intents = discordgo.IntentsAllWithoutPrivileged | discordgo.IntentsMessageContent

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

			switch data.CustomID {
			case "tikm":
				topicId := data.Values[0]
				fmt.Println("TicketCreate:", topicId)

				// Create new ticket under ticket channel via private threads
				topic, ok := topics[topicId]

				if !ok {
					fmt.Println("Invalid topic ID:", topicId)
					return
				}

				modalqas := make([]discordgo.MessageComponent, len(topic.Questions))

				for i, question := range topic.Questions {
					modalqas[i] = discordgo.ActionsRow{
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
						CustomID:   "tikmodal",
						Title:      topic.Name,
						Components: modalqas,
					},
				})

				if err != nil {
					fmt.Println("Error:", err)
				}

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
			}
		case discordgo.InteractionModalSubmit:
			modalData := i.ModalSubmitData()

			// Create the thread
			/*thread, err := s.ThreadStartComplex(os.Getenv("TICKET_THREAD_CHANNEL=1053875909024817182"), &discordgo.ThreadStart{

			}) */
		}
	})

	err = discord.Open()

	if err != nil {
		panic(err)
	}

	select {}
}
