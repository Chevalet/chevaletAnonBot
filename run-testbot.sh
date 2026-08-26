#!/usr/bin/env bash
# Runs the bot against the TEST bot token + throwaway Postgres (see .env.test).
# Both are git-ignored; this script is safe to keep but never points at prod.
set -a; . ./.env.test; set +a
exec go run ./cmd/bot
