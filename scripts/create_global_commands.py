import requests
import getpass

app = input("Application id: ")
token = getpass.getpass(prompt="Bot token: ")

CHAT_INPUT = 1
GUILD_INSTALL = 0
CTX_GUILD = 0
CTX_BOT_DM = 1
OPT_STRING = 3
OPT_BOOL = 5
ADMINISTRATOR_PERM = 0x0000000000000008
MANAGER_PERM = 0x0000000000000020

headers = {"Authorization": f"Bot {token}"}

url = f"https://discord.com/api/v10/applications/{app}/commands"

json_bootstrap = {
    "name": "bootstrap",
    "type": CHAT_INPUT,
    "description": "Register LSDC2 bot commands in your guild",
    "integration_types": [GUILD_INSTALL],
    "contexts": [CTX_GUILD],
    "default_member_permissions": ADMINISTRATOR_PERM,
}
r = requests.post(url, headers=headers, json=json_bootstrap)
print("BOOSTRAP result: ", r.content)

json_registergame = {
    "name": "register-game",
    "type": CHAT_INPUT,
    "description": "Add a new game in the LSDC2 launcher",
    "integration_types": [GUILD_INSTALL],
    "contexts": [CTX_BOT_DM],
    "default_member_permissions": ADMINISTRATOR_PERM,
    "options": [
        {
            "type": OPT_STRING,
            "name": "spec-url",
            "description": "Url to LSDC2-compatible game description",
        },
        {
            "type": OPT_BOOL,
            "name": "overwrite",
            "description": "If true, overwrite any existing spec",
        },
    ],
}
r = requests.post(url, headers=headers, json=json_registergame)
print("REGISTER-GAME result: ", r.content)
