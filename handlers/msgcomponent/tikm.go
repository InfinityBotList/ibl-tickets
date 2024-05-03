package msgcomponent

import (
	"context"
	"fmt"
	"ibl-tickets/types"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func tikm(s *discordgo.Session, i *discordgo.Interaction, data discordgo.MessageComponentInteractionData, config *types.Config, pool *pgxpool.Pool, ctx context.Context, logger *zap.Logger, rediscli *redis.Client) error {
	// Edit existing message to reset the select menu
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Embeds:     &i.Message.Embeds,
		Components: &i.Message.Components,
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
		return fmt.Errorf("topic not found")
	}

	// Check cooldown from redis
	cooldownKey := "ticket_cooldown:" + i.Member.User.ID

	cooldown := rediscli.TTL(ctx, cooldownKey).Val()

	if cooldown == -2 || cooldown == -1 {
		// Set cooldown
		err = rediscli.Set(ctx, cooldownKey, "0", 10*time.Second).Err()

		if err != nil {
			logger.Error("Error setting cooldown", zap.Error(err), zap.String("userId", i.Member.User.ID))
			return fmt.Errorf("error setting cooldown: %w", err)
		}
	} else {
		// Cooldown exists
		logger.Info("User is on cooldown", zap.String("userId", i.Member.User.ID), zap.Duration("cooldown", cooldown))

		s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "You are on cooldown. Please wait ``" + cooldown.String() + "`` before creating another ticket.",
				Flags:   discordgo.MessageFlagsEphemeral,
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse: []discordgo.AllowedMentionType{},
				},
			},
		})
		return nil
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

	err = s.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID:   "tikmodal:" + topicId,
			Title:      topic.Name,
			Components: modalqas,
		},
	})

	if err != nil {
		logger.Error("Error sending message", zap.Error(err), zap.String("userId", i.Member.User.ID))
		return fmt.Errorf("error sending message: %w", err)
	}

	return nil
}
