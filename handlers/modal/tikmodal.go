package modal

import (
	"context"
	"fmt"
	"ibl-tickets/types"
	"ibl-tickets/utils"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/infinitybotlist/eureka/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func _deleteThread(pool *pgxpool.Pool, ctx context.Context, s *discordgo.Session, threadId string, tikId string) error {
	_, err := pool.Exec(ctx, "DELETE FROM tickets WHERE id = $1", tikId)

	if err != nil {
		return fmt.Errorf("error deleting ticket from database: %w", err)
	}

	_, err = s.ChannelDelete(threadId)

	if err != nil {
		return fmt.Errorf("error deleting thread: %w", err)
	}

	return nil
}

func tikModal(s *discordgo.Session, i *discordgo.Interaction, data discordgo.ModalSubmitInteractionData, config *types.Config, pool *pgxpool.Pool, ctx context.Context, logger *zap.Logger, rediscli *redis.Client) error {
	topicId := strings.Split(data.CustomID, ":")[1]

	topic, ok := config.Topics[topicId]

	if !ok {
		return fmt.Errorf("topic not found")
	}

	// Send a message to the user
	err := s.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Creating ticket.\n\nPlease wait...",
			Flags:   discordgo.MessageFlagsEphemeral,
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		},
	})

	if err != nil {
		return fmt.Errorf("error sending create response: %w", err)
	}

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
			return fmt.Errorf("error converting question number to int: %w", err)
		}

		answers[topic.Questions[questionNum].Question] = input.Value
	}

	thread, err := s.ThreadStartComplex(config.Channels.ThreadChannel, &discordgo.ThreadStart{
		Name: issue,
		Type: discordgo.ChannelTypeGuildPrivateThread,
	})

	if err != nil {
		return fmt.Errorf("error creating thread: %w", err)
	}

	tikId := crypto.RandString(64)

	// Add the ticket to the database
	_, err = pool.Exec(ctx, "INSERT INTO tickets (id, user_id, channel_id, topic_id, ticket_context, issue) VALUES ($1, $2, $3, $4, $5, $6)", tikId, i.Member.User.ID, thread.ID, topicId, answers, issue)

	if err != nil {
		logger.Error("Error inserting ticket into database", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId))
		return fmt.Errorf("error inserting ticket into database: %w", err)
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
				Title:       "Ticket created by " + i.Member.User.Username + "(" + i.Member.User.GlobalName + ")",
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

		delThreadErr := _deleteThread(pool, ctx, s, thread.ID, tikId)

		if err != nil {
			logger.Error("Error deleting thread", zap.Error(delThreadErr), zap.String("issue", issue), zap.String("topicId", topicId), zap.String("userId", i.Member.User.ID))
		}
		return fmt.Errorf("error sending message: %w", err)
	}

	err = s.ThreadMemberAdd(thread.ID, i.Member.User.ID)

	if err != nil {
		logger.Error("Error adding user to thread", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId), zap.String("userId", i.Member.User.ID))
		err = _deleteThread(pool, ctx, s, thread.ID, tikId)

		if err != nil {
			logger.Error("Error deleting thread", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId), zap.String("userId", i.Member.User.ID))
		}

		return fmt.Errorf("error adding user to thread: %w", err)
	}

	// Pin the message
	err = s.ChannelMessagePin(thread.ID, m.ID)

	if err != nil {
		logger.Error("Error pinning message", zap.Error(err), zap.String("issue", issue), zap.String("topicId", topicId))
		return fmt.Errorf("failed to pin start message: %w", err)
	}

	// Send a message to the user
	s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
		Content: utils.Stringp("Your ticket has been created! You can view it here: (https://discord.com/channels/" + i.GuildID + "/" + thread.ID + ")"),
	})

	return nil
}
