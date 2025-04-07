import requests

app = 
token = 
guild_id = 
cmd_name = 

CHAT_INPUT = 1
GUILD_INSTALL = 0
CTX_GUILD = 0
CTX_BOT_DM = 1
OPT_STRING = 3
OPT_INT = 4
OPT_BOOL = 5
OPT_USER = 6
OPT_CHANNEL = 7
OPT_ROLE = 8
OPT_MENTIONABLE = 9
OPT_NUMBER = 10
OPT_ATTACHMENT = 11
ADMINISTRATOR_PERM = 0x0000000000000008
MANAGER_PERM = 0x0000000000000020

headers = {"Authorization": f"Bot {token}"}

url = f"https://discord.com/api/v10/applications/{app}/guilds/{guild_id}/commands"
jbody = requests.get(url, headers=headers).json()

for cmd in jbody:
    if cmd["name"] == cmd_name:
        # do somthing with cmd
        url = f"https://discord.com/api/v10/applications/{app}/guilds/{guild_id}/commands/{cmd['id']}"
        r = requests.patch(url, headers=headers, json=cmd)
        print("UPDATE result: ", r.content)
        break
