#!/bin/bash

# Prompt for Discord bot ID and secrets
printf "Bot Application ID (General Information panel): "
read APP_ID

printf "Bot Token (Bot panel): "
read -s BOT_TOKEN
printf "\n"

BASE_URL="https://discord.com/api/v10/applications/$APP_ID/commands"
AUTH_HEADER="Authorization: Bot $BOT_TOKEN"

# Fetch all global commands
response=$(curl -s -H "$AUTH_HEADER" "$BASE_URL")

# Count and display the number of commands
command_count=$(echo "$response" | jq '. | length')
echo "Number of global commands: $command_count"

# Iterate over the commands
echo "$response" | jq -c '.[]' | while read -r cmd; do
  cmd_id=$(echo "$cmd" | jq -r '.id')
  cmd_name=$(echo "$cmd" | jq -r '.name')
  echo -n "Deleting command: $cmd_name"
  delete_response=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE -H "$AUTH_HEADER" "$BASE_URL/$cmd_id")
  echo " - HTTP $delete_response"
done
