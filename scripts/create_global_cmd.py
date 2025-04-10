import requests
import getpass

app = 
token = 

CHAT_INPUT = 1
USER = 2
GUILD_INSTALL = 0
CTX_GUILD = 0
CTX_BOT_DM = 1
OPT_STRING = 3
OPT_BOOL = 5
ADMINISTRATOR_PERM = 0x0000000000000008
MANAGER_PERM = 0x0000000000000020

headers = {"Authorization": f"Bot {token}"}

url = f"https://discord.com/api/v10/applications/{app}/commands"

json_welcomeguild = {
    "name": "welcome-guild",
    "type": CHAT_INPUT,
    "description": "Deploy LSDC2 bot commands, channels and roles in your guild",
    "integration_types": [GUILD_INSTALL],
    "contexts": [CTX_GUILD],
    "default_member_permissions": ADMINISTRATOR_PERM,
}
r = requests.post(url, headers=headers, json=json_welcomeguild)
print("WELCOME-GUILD result: ", r.content)

json_goodbyeguild = {
    "name": "goodbye-guild",
    "type": CHAT_INPUT,
    "description": "Remove everything related to LSDC2 bot from your guild",
    "integration_types": [GUILD_INSTALL],
    "contexts": [CTX_GUILD],
    "default_member_permissions": ADMINISTRATOR_PERM,
}
r = requests.post(url, headers=headers, json=json_goodbyeguild)
print("GOODBYE-GUILD result: ", r.content)

json_registergame = {
    "name": "register-game",
    "type": CHAT_INPUT,
    "description": "Add a new game in the LSDC2 launcher",
    "integration_types": [GUILD_INSTALL],
    "contexts": [CTX_BOT_DM],
    "default_member_permissions": ADMINISTRATOR_PERM,
    "options": [
        {
            "type": OPT_BOOL,
            "name": "overwrite",
            "description": "If true, overwrite any existing spec",
        },
    ],
}
r = requests.post(url, headers=headers, json=json_registergame)
print("REGISTER-GAME result: ", r.content)

json_registerenginetier = {
    "name": "register-engine-tier",
    "type": CHAT_INPUT,
    "description": "Add/update a engine tier in the LSDC2 launcher",
    "integration_types": [GUILD_INSTALL],
    "contexts": [CTX_BOT_DM],
    "default_member_permissions": ADMINISTRATOR_PERM,
}
r = requests.post(url, headers=headers, json=json_registerenginetier)
print("REGISTER-ENGINE-TIER result: ", r.content)
