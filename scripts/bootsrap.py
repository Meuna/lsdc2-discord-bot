import requests
import getpass

app = input("Application id: ")
token = getpass.getpass(prompt="Bot token: ")

ADMINISTRATOR_PERM = 0x0000000000000008
MANAGER_PERM = 0x0000000000000020

headers = {"Authorization": f"Bot {token}"}

url = f"https://discord.com/api/v10/applications/{app}/commands"

json_bootstrap = {
    "name": "bootstrap",
    "type": 1,
    "description": "Register LSDC2 bot commands in your guild",
    "default_member_permissions": ADMINISTRATOR_PERM,
}
r = requests.post(url, headers=headers, json=json_bootstrap)
print("BOOSTRAP result: ", r.content)

json_registergame = {
    "name": "register-game",
    "type": 1,
    "description": "Add a new game in the LSDC2 launcher",
    "default_member_permissions": ADMINISTRATOR_PERM,
    "dm_permission": True,
    "options": [
        {
            "type": 3,
            "name": "spec-url",
            "description": "Url to LSDC2-compatible game description",
        },
        {
            "type": 5,
            "name": "overwrite",
            "description": "If true, overwrite any existing spec",
        },
    ],
}
r = requests.post(url, headers=headers, json=json_registergame)
print("ADD-GAME result: ", r.content)

json_spinupgame = {
    "name": "spinup",
    "description": "Start a new server instance",
    "default_member_permissions": MANAGER_PERM,
    "options": [
        {
            "type": 3,
            "name": "game-type",
            "description": "Game type to start",
            "required": True,
        },
    ],
}
r = requests.post(url, headers=headers, json=json_spinupgame)
print("ADD-GAME result: ", r.content)
