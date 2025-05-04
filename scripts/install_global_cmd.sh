#!/bin/bash

# Prompt for Discord bot ID and secrets
printf "Bot Application ID (General Information panel): "
read APP_ID

printf "Bot Token (Bot panel): "
read -s BOT_TOKEN
printf "\n"

BASE_URL="https://discord.com/api/v10/applications/$APP_ID/commands"
AUTH_HEADER="Authorization: Bot $BOT_TOKEN"

# Function to send a POST request
send_request() {
  local json_payload=$1
  local cmd_name=$2
  echo -n "Installing command: $cmd_name"
  http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "$AUTH_HEADER" -H "Content-Type: application/json" -d "$json_payload" "$BASE_URL")
  echo " - HTTP $http_code"
}

# JSON payloads for commands
JSON_WELCOME_GUILD='{
  "name": "welcome-guild",
  "type": 1,
  "description": "Deploy LSDC2 bot commands, channels and roles in your guild",
  "integration_types": [0],
  "contexts": [0],
  "default_member_permissions": "8"
}'

JSON_GOODBYE_GUILD='{
  "name": "goodbye-guild",
  "type": 1,
  "description": "Remove everything related to LSDC2 bot from your guild",
  "integration_types": [0],
  "contexts": [0],
  "default_member_permissions": "8"
}'

JSON_REGISTER_GAME='{
  "name": "register-game",
  "type": 1,
  "description": "Add a new game in the LSDC2 launcher",
  "integration_types": [0],
  "contexts": [1],
  "default_member_permissions": "8",
  "options": [
    {
      "type": 5,
      "name": "overwrite",
      "description": "If true, overwrite any existing spec"
    }
  ]
}'

JSON_REGISTER_ENGINE_TIER='{
  "name": "register-engine-tier",
  "type": 1,
  "description": "Add/update an engine tier in the LSDC2 launcher",
  "integration_types": [0],
  "contexts": [1],
  "default_member_permissions": "8"
}'

# Send requests for each command
send_request "$JSON_WELCOME_GUILD" "welcome-guild"
send_request "$JSON_GOODBYE_GUILD" "goodbye-guild"
send_request "$JSON_REGISTER_GAME" "register-game"
send_request "$JSON_REGISTER_ENGINE_TIER" "register-engine-tier"
