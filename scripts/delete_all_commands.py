import requests
import getpass

app = input("Application id: ")
guild = input("Guild id: ")
token = getpass.getpass(prompt="Bot token: ")

headers = {"Authorization": f"Bot {token}"}

# TODO: implement 429 aware rate lmimiter

# Remove app commands
url = f"https://discord.com/api/v10/applications/{app}/commands"
jbody = requests.get(url, headers=headers).json()
print(f"Number of global commands: {len(jbody)}")

for cmd in jbody:
    print(f"Deleting command: {cmd['name']}")
    cmd_url = url + "/" + cmd["id"]
    r = requests.delete(cmd_url, headers=headers)

# Remove guild commands
url = f"https://discord.com/api/v10/applications/{app}/guilds/{guild}/commands"
jbody = requests.get(url, headers=headers).json()
print(f"Number of guilds commands: {len(jbody)}")

for cmd in jbody:
    print(f"Deleting command: {cmd['name']}")
    cmd_url = url + "/" + cmd["id"]
    r = requests.delete(cmd_url, headers=headers)
