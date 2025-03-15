import requests

app =
token =
guild_id =

CHAT_INPUT = 1
GUILD_INSTALL = 0
CTX_GUILD = 0
CTX_BOT_DM = 1
OPT_STRING = 3
OPT_INT = 4
OPT_BOOL = 5
ADMINISTRATOR_PERM = 0x0000000000000008
MANAGER_PERM = 0x0000000000000020

headers = {"Authorization": f"Bot {token}"}

url = f"https://discord.com/api/v10/applications/{app}/guilds/{guild_id}/commands"

json_cmd = {
    "name": ,
    "type": CHAT_INPUT,
    "description": ,
}

r = requests.post(url, headers=headers, json=json_cmd)
print("Result: ", r.content)
