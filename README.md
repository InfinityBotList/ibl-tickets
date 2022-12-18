# ibl-tickets

Custom ticketting bot for IBL v4. Self-hosting is not officially supported.

**Licensed under the MIT License.**

## Getting started

*The bot must be in a team to use this.*

- Fill out `.env` as per `.env.sample`. Also edit `topics.yaml` with the topics that tickets can be,
- Run `make` to build the executable. Go 1.19 is required.
- Run `./ibl-tickets` to start the bot.

## Create the ticket message

In the `TICKET_CREATE_CHANNEL` defined in `.env`, type `tikm`. You must be a owner of the bot to do this.