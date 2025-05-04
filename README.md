# lsdc2-discord-bot

This project is part of the LSDC2 stack. *LSDC* stands for *"Le serveur des copains"*
which can be translated to *"The Pals' Server"*. It is an AWS hosted Discord bot to
provision short lived Spot game server.

This project is the software running the logic of the Discord bot. The bot is
implemented serverless, using the Discord HTTP API. Long running logic (> 3 sec) are
sent to an SQS queue. As a result, the bot is implemented in 2 pieces:
* The frontend: implement the Discord Interactions Endpoint and the fast logic.
* The backend: implement asynchronous, slow logic, and notifications from
EventBridge and lsdc2-pilot.

The high-level interactions of the bot is illustrated below.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="doc/arch-dark.svg">
  <img alt="LSDC2 stack high level architecture" src="doc/arch-light.svg">
</picture>

## Deployment

### Clone the lsdc2-discord-bot and lsdc2-cdk

```console
$ git clone https://github.com/Meuna/lsdc2-discord-bot.git
$ git clone https://github.com/Meuna/lsdc2-cdk.git
```

### Build the Lambda binaries

```console
$ cd lsdc2-discord-bot
$ scripts/build.sh
```

This will generate the files `frontend.zip` and `backend.zip` at the root of the
working directory.

### Deploy the CDK stack

Follow the instructions here: https://github.com/Meuna/lsdc2-cdk/blob/main/README.md#usage

### Bootstrap bot global commands

At this stage, the LSDC2 bot is online but does not have any command. Run the
`scripts/install_global_cmd.sh` script to install the bot global commands.
Successful installation returns the code 201.

```console
$ scripts/install_global_cmd.sh
Bot Application ID (General Information panel): ...
Bot Token (Bot panel): 
Installing command: welcome-guild - HTTP 201
Installing command: goodbye-guild - HTTP 201
Installing command: register-game - HTTP 201
Installing command: register-engine-tier - HTTP 201
```

### Discord guild enrolment

You first need to invite the LSDC2 bot to your server. One way to do this is to
generate an OAuth2 URL (OAuth2/OAuth2 URL Generator panel). The bot requires the
`bot` scope, with the `Administrator` permission.

Generate a link, paste it into your browser and select the server you want the
LSDC2 bot to join.

After the bot joined the server, run the **/welcome-guild** command in any channel,
from as a server admin. After a minute, you should see the creation of the
LSDC2 channels `administration` and `welcome`, and a `✅ Welcome complete !`
success message.

The bot is now fully deployed on your server !

## Usage

### Bot owner commands

The commands are run as bot direct message, by the bot owner (matching the
User ID recorded in the `/lsdc2/discord-secrets` parameter).

| **Command**                    | **Description**
| ------------------------------ | ---------------
| **/register-game** `overwrite` | The LSDC2 bot returns a JSON prompt to add a new `game-type` handled by the bot (see [Game spec](#game-type-specification)). The `overwrite` parameter is used to replace the `game-type` entry if it already exists.
| **/register-engine-tier**      | The LSDC2 bot returns a JSON prompt to add a new `engine-tier` handled by the bot (see [Engine spec](#server-engine-specification)).

### Server admin commands

The commands are run as a server admin, from any server channel.

| **Command**                    | **Description**
| ------------------------------ | ---------------
| **/welcome-guild**             | Deploy LSDC2 bot commands, channels and roles in the server.
| **/goodbye-guild**             | Remove everything related to LSDC2 bot from your guild.

### LSDC2 Admin commands

Unless stated otherwise, the LSDC2 Admin are run from the LSDC2 admin channel,
by an LSDC2 admin.

| **Command**                | **Description**
| -------------------------- | ---------------
| **/spinup** `game-type`    | Create a new server for the `game-type`. After a few moment, a dedicated server channel will be created.
| **/conf** `server-channel` | Open the game configuration for the `server-channel`.
| **/destroy** `server-name` | Destroy the server with the name `server-name`.
| **/invite** `member`       | Assign the LSDC2 User role to the `member`. If run from a server channel, the `member` is also added to the server channel.
| **/kick** `member`         | Remove the LSDC2 User role from the `member`. If run from a server channel, the `member` is only removed from the server channel.

### LSDC2 User functions

The commands here are run from a server channel (created with the admin command
`/spinup`), by a LSDC2 User.

| **Command**          | **Description**
| -------------------- | ---------------
| **/start**           | Start the server.
| **/stop**            | Stop the server.
| **/status**          | Return the status of the server.
| **/download**        | Return a link to download the server game files.
| **/upload**          | Return a link to upload game files to the server.
| **/invite** `member` | Add the `member` to the server channel if he already is an LSDC2 User.

## Game type specification

Running the **/register-game** command returns a prompt to provide a JSON document
with a game specification. The JSON document is an object, with the following
properties:

```json
{
    "name": "string",       // Required: unique identifier of the game-spec
    "engineType": "enum",   // Required: ecs, ec2
    "engine": {},           // Required: schema depends on engineType value
    "ingress": {            // Required: at least 1 key tcp/udp
        "udp": ["integer"], //   list of UDP ingress ports
        "tcp": ["integer"]  //   list of TCP ingress ports
    },
    "env": {                // Optional: key/value of environment variables
        "string": "string"
    },
    "params": {             // Optional: key/label of parameters
        "string": "string"
    }
}
```

The difference between the keys `env` and `params` is as follow:
* `env` values are statically set to the game server.
* `params` values are provided by the user when using the **/spinup** or
**/conf** commands.

The key `engine` has the following schema when `engineType` = `ecs`:

```json
{
    "image": "string",      // Required: uri of the container image running the game
    "cpu": "string",        // Required: number of vCPU, formatted according to AWS SDK (e.g "2 vCPU")
    "memory": "string",     // Required: ram, formatted according to AWS SDK (e.g. "8 GB")
    "storage": "integer"    // Optional: storage of the ECS task in GiB (between 21 and 200)
}
```

> [!IMPORTANT]  
> The `cpu` and `memory` must be valid Fargate combination. Refer to ECS task
> definition invalid CPU or memory documentation.

The key `engine` has the following schema when `engineType` = `ec2`:

```json
{
    "ami": "string",                // Required: name of the AMI running the game
    "instanceTypes": ["string"],    // Required: list of instance types capable of running the game (e.g "t2.medium")
    "iops": "integer",              // Optional: specify the IOPS performance of the EBS volume
    "throughput": "integer",        // Optional: specify the throughput performance of the EBS volume
    "fastboot": "boolean"           // Optional: if true, the volume performance is restored to its default values after the game is ready
}
```

> [!IMPORTANT]  
> Valid `iops` and `throughput` values depend on the AMI default size. Refer to
> AWS gp3 volume performance documentation.

## Game type specification examples

### Example with ECS engine

```json
{
    "name": "sevendtd-ecs",
    "engineType": "ecs",
    "engine": {
        "image": "meuna/lsdc2:sevendtd",
        "cpu": "2 vCPU",
        "memory": "8 GB",
        "storage": 25
    },
    "ingress": {
        "tcp": [26900],
        "udp": [26901, 26902, 26903, 26904, 26905]
    },
    "env": {
        "LSDC2_LOW_MEMORY_WARNING_MB": "2048",
        "LSDC2_LOW_MEMORY_SIGNAL_MB": "1024",
        "LSDC2_SCAN_STDOUT": "true",
        "LSDC2_WAKEUP_SENTINEL": "GameServer.LogOn successful"
    },
    "params": {
        "ADMIN_STEAMID": "Steam ID of the admin",
        "SERVER_PASS": "Password",
        "GAME_DAYLENGTH": "Day length (default 60)",
        "GAME_DIFFICULTY": "Difficulty (0-[1]-2-3-4-5)",
        "SERVER_ALLOW_CROSSPLAY": "Crossplay & EAC"
    }
}
```

### Example with EC2 engine

```json
{
    "name": "sevendtd-ec2",
    "engineType": "ec2",
    "engine": {
        "ami": "lsdc2/images/sevendtd",
        "instanceTypes": ["r7i.large", "r6i.large", "r5.large", "r5a.large", "r5d.large", "r5ad.large", "m6a.xlarge"],
        "iops": 6000,
        "throughput": 400,
        "fastboot": true
    },
    "ingress": {
        "tcp": [26900],
        "udp": [26901, 26902, 26903, 26904, 26905]
    },
    "env": {
        "LSDC2_LOW_MEMORY_WARNING_MB": "2048",
        "LSDC2_LOW_MEMORY_SIGNAL_MB": "1024",
        "LSDC2_SCAN_STDOUT": "true",
        "LSDC2_WAKEUP_SENTINEL": "GameServer.LogOn successful"
    },
    "params": {
        "ADMIN_STEAMID": "Steam ID of the admin",
        "SERVER_PASS": "Password",
        "GAME_DAYLENGTH": "Day length (default 60)",
        "GAME_DIFFICULTY": "Difficulty (0-[1]-2-3-4-5)",
        "SERVER_ALLOW_CROSSPLAY": "Crossplay & EAC"
    }
}
```

## Server engine specification

Running the **/register-engine-tier** command returns a prompt to provide a JSON
document with a engine spec. The JSON document is either an object, or a list of
objects, with the following properties:

```json
{
    "name": "string",               // Required: unique identifier of the engine-spec
    "cpu": "string",                // Required: number of vCPU, formatted according to AWS SDK (e.g "2 vCPU")
    "memory": "string",             // Required: ram, formatted according to AWS SDK (e.g. "8 GB")
    "instanceTypes": ["string"],    // Required: list of instance types capable of running the game (e.g "t2.medium")
}
```

The [server-tiers.json](server-tiers.json) file provide readily available game spec.
