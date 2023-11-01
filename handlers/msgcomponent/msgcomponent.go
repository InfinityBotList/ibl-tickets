package msgcomponent

import (
	"context"
	"ibl-tickets/types"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

var Handlers = map[string]func(s *discordgo.Session, i *discordgo.Interaction, data discordgo.MessageComponentInteractionData, config *types.Config, pool *pgxpool.Pool, ctx context.Context, logger *zap.Logger, rediscli *redis.Client) error{}

func AddHandler(name string, handler func(s *discordgo.Session, i *discordgo.Interaction, data discordgo.MessageComponentInteractionData, config *types.Config, pool *pgxpool.Pool, ctx context.Context, logger *zap.Logger, rediscli *redis.Client) error) {
	Handlers[name] = handler
}

func init() {
	AddHandler("tikm", tikm)
	AddHandler("close", close)
}
