package internal

import (
	"encoding/json"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
	"github.com/bwmarrin/discordgo"
)

const (
	RegisterGameAPI string = "register-game"
	BootstrapAPI    string = "bootstrap"
	SpinupAPI       string = "spinup"
	DestroyAPI      string = "destroy"
	StartAPI        string = "start"
	StopAPI         string = "stop"
	StatusAPI       string = "status"
	DownloadAPI     string = "download"
	UploadAPI       string = "upload"
)

const (
	RegisterGameAPISpecUrlOpt   string = "spec-url"
	RegisterGameAPIOverwriteOpt string = "overwrite"
)

type BackendCmd struct {
	Args  interface{}
	Token string `json:",omitempty"`
	AppID string `json:",omitempty"`
}

func (cmd BackendCmd) Action() string {
	switch cmd.Args.(type) {
	case *RegisterGameArgs, RegisterGameArgs:
		return RegisterGameAPI
	case *BootstrapArgs, BootstrapArgs:
		return BootstrapAPI
	case *SpinupArgs, SpinupArgs:
		return SpinupAPI
	case *DestroyArgs, DestroyArgs:
		return DestroyAPI
	default:
		panic(fmt.Sprintf("Incompatible BackendCmd Args type %T", cmd.Args))
	}
}

func (cmd *BackendCmd) UnmarshalJSON(src []byte) error {
	type backendCmd BackendCmd
	var tmp struct {
		backendCmd
		Action string
		Args   json.RawMessage
	}
	err := json.Unmarshal(src, &tmp)
	if err != nil {
		return err
	}

	*cmd = BackendCmd(tmp.backendCmd)

	switch tmp.Action {
	case RegisterGameAPI:
		cmd.Args = &RegisterGameArgs{}
	case BootstrapAPI:
		cmd.Args = &BootstrapArgs{}
	case SpinupAPI:
		cmd.Args = &SpinupArgs{}
	case DestroyAPI:
		cmd.Args = &DestroyArgs{}
	default:
		return fmt.Errorf("unknown command: %s", tmp.Action)
	}

	return json.Unmarshal(tmp.Args, cmd.Args)
}

func (cmd BackendCmd) MarshalJSON() ([]byte, error) {
	type backendCmd BackendCmd
	return json.Marshal(struct {
		backendCmd
		Action string
	}{
		backendCmd: backendCmd(cmd),
		Action:     cmd.Action(),
	})
}

type RegisterGameArgs struct {
	SpecUrl   string `json:",omitempty"`
	Spec      string `json:",omitempty"`
	Overwrite bool
}

type BootstrapArgs struct {
	GuildID string
}

type SpinupArgs struct {
	GameName string
	GuildID  string
	Env      map[string]string
}

type DestroyArgs struct {
	ChannelID string
}

func QueueMarshalledAction(queueUrl string, cmd BackendCmd) error {
	bodyBytes, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	return QueueMessage(queueUrl, string(bodyBytes[:]))
}

func UnmarshallQueuedAction(record events.SQSMessage) (BackendCmd, error) {
	cmd := BackendCmd{}
	err := json.Unmarshal([]byte(record.Body), &cmd)
	return cmd, err
}

func MarshalCustomIDAction(cmd BackendCmd) (string, error) {
	bodyBytes, err := json.Marshal(cmd)
	return string(bodyBytes[:]), err
}

func UnmarshallCustomIDAction(customID string) (BackendCmd, error) {
	cmd := BackendCmd{}
	err := json.Unmarshal([]byte(customID), &cmd)
	return cmd, err
}

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        SpinupAPI,
		Description: "Start a new server instance",
		// DefaultMemberPermissions: &discordgo.PermissionManageServer,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "game-type",
				Description: "Game type to start",
				Required:    true,
				Type:        discordgo.ApplicationCommandOptionString,
			},
		},
	},
	{
		Name:        DestroyAPI,
		Description: "Destroy a server",
		// DefaultMemberPermissions: &discordgo.PermissionManageServer,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "server-name",
				Description: "The name of the server to destroy",
				Required:    true,
				Type:        discordgo.ApplicationCommandOptionString,
			},
		},
	},
	{
		Name:        StartAPI,
		Description: "Start a server instance (run in instance channel)",
	},
	{
		Name:        StopAPI,
		Description: "Stop a running server instance (run in instance channel)",
	},
	{
		Name:        StatusAPI,
		Description: "Give the status of a server instance (run in instance channel)",
	},
	{
		Name:        DownloadAPI,
		Description: "Retrieve the savegame of a server instance (run in instance channel)",
	},
	{
		Name:        UploadAPI,
		Description: "Upload a savegame to a server instance (run in instance channel)",
	},
}

func CreateGuildCommands(sess *discordgo.Session, appID string, guildID string) error {
	for _, cmd := range slashCommands {
		fmt.Printf("Bootstraping %s: %s command\n", guildID, cmd.Name)
		_, err := sess.ApplicationCommandCreate(appID, guildID, cmd)
		if err != nil {
			return err
		}
	}
	return nil
}
