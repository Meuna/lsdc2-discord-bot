package internal

import (
	"encoding/json"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

const (
	RegisterGameAPI       = "register-game"
	RegisterEngineTierAPI = "register-engine-tier"
	WelcomeAPI            = "welcome-guild"
	GoodbyeAPI            = "goodbye-guild"
	SpinupAPI             = "spinup"
	ConfAPI               = "conf"
	DestroyAPI            = "destroy"
	InviteAPI             = "invite"
	KickAPI               = "kick"
	StartAPI              = "start"
	StopAPI               = "stop"
	StatusAPI             = "status"
	DownloadAPI           = "download"
	UploadAPI             = "upload"
	TaskNotifyAPI         = "tasknotify"
)

var (
	OwnerCmd      = []string{RegisterGameAPI, RegisterEngineTierAPI, WelcomeAPI, GoodbyeAPI}
	AdminCmd      = []string{SpinupAPI, DestroyAPI, InviteAPI, KickAPI, ConfAPI}
	InviteKickCmd = []string{InviteAPI, KickAPI}
	UserCmd       = []string{StartAPI, StopAPI, StatusAPI, DownloadAPI, UploadAPI}
)

// BackendCmd represents a command exchanged between the frontend and backend,
// or during frontend roundtrips (e.g., modals and message components).
// The Api field identifies the command type, and Args holds the command-specific data.
type BackendCmd struct {
	AppID string `json:",omitempty"`
	Token string `json:",omitempty"`
	Api   string
	Args  any
}

// UnmarshalJSON customizes JSON unmarshalling for BackendCmd. It determines the
// Args type based on the Api field and unmarshals the Args accordingly.
func (cmd *BackendCmd) UnmarshalJSON(src []byte) error {
	type Alias BackendCmd
	var aux struct {
		Alias
		Args json.RawMessage
	}

	err := json.Unmarshal(src, &aux)
	if err != nil {
		return err
	}

	*cmd = BackendCmd(aux.Alias)

	switch aux.Api {
	case RegisterGameAPI:
		cmd.Args = &RegisterGameArgs{}
	case RegisterEngineTierAPI:
		cmd.Args = &RegisterEngineTierArgs{}
	case WelcomeAPI:
		cmd.Args = &WelcomeArgs{}
	case GoodbyeAPI:
		cmd.Args = &GoodbyeArgs{}
	case SpinupAPI:
		cmd.Args = &SpinupArgs{}
	case ConfAPI:
		cmd.Args = &ConfArgs{}
	case StartAPI:
		cmd.Args = &StartArgs{}
	case DestroyAPI:
		cmd.Args = &DestroyArgs{}
	case InviteAPI:
		cmd.Args = &InviteArgs{}
	case KickAPI:
		cmd.Args = &KickArgs{}
	case TaskNotifyAPI:
		cmd.Args = &TaskNotifyArgs{}
	default:
		return fmt.Errorf("unknown command: %s", aux.Api)
	}

	return json.Unmarshal(aux.Args, cmd.Args)
}

// MarshalJSON customizes JSON marshalling for BackendCmd. It sets the Api field
// based on the Args type and marshals the command to JSON.
func (cmd BackendCmd) MarshalJSON() ([]byte, error) {
	type backendCmd BackendCmd

	switch cmd.Args.(type) {
	case *RegisterGameArgs, RegisterGameArgs:
		cmd.Api = RegisterGameAPI
	case *RegisterEngineTierArgs, RegisterEngineTierArgs:
		cmd.Api = RegisterEngineTierAPI
	case *WelcomeArgs, WelcomeArgs:
		cmd.Api = WelcomeAPI
	case *GoodbyeArgs, GoodbyeArgs:
		cmd.Api = GoodbyeAPI
	case *SpinupArgs, SpinupArgs:
		cmd.Api = SpinupAPI
	case *ConfArgs, ConfArgs:
		cmd.Api = ConfAPI
	case *StartArgs, StartArgs:
		cmd.Api = StartAPI
	case *DestroyArgs, DestroyArgs:
		cmd.Api = DestroyAPI
	case *InviteArgs, InviteArgs:
		cmd.Api = InviteAPI
	case *KickArgs, KickArgs:
		cmd.Api = KickAPI
	case *TaskNotifyArgs, TaskNotifyArgs:
		cmd.Api = TaskNotifyAPI
	default:
		return nil, fmt.Errorf("incompatible BackendCmd Args type %T", cmd.Args)
	}

	return json.Marshal(backendCmd(cmd))
}

type RegisterGameArgs struct {
	Spec      string `json:",omitempty"`
	Overwrite bool
}

type RegisterEngineTierArgs struct {
	Spec string `json:",omitempty"`
}

type WelcomeArgs struct {
	GuildID string
}

type GoodbyeArgs struct {
	GuildID string
}

type SpinupArgs struct {
	GameName string
	GuildID  string
	Env      map[string]string
}

type ConfArgs struct {
	ChannelID string
	Env       map[string]string
}

type StartArgs struct {
	ChannelID  string
	EngineTier string
}

type DestroyArgs struct {
	ChannelID string
}

type InviteArgs struct {
	GuildID          string
	ChannelID        string
	RequesterID      string
	TargetID         string
	RequesterIsAdmin bool
}

type KickArgs struct {
	GuildID          string
	ChannelID        string
	RequesterID      string
	TargetID         string
	RequesterIsAdmin bool
}

type TaskNotifyArgs struct {
	ServerName string
	Action     string
	Message    string
}

// QueueMarshalledCmd marshals a BackendCmd to JSON and sends it to the
// specified queue URL. Returns an error if marshalling or sending fails.
func QueueMarshalledCmd(queueUrl string, cmd BackendCmd) error {
	bodyBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("json.Marshal / %w", err)
	}
	return QueueMessage(queueUrl, string(bodyBytes[:]))
}

// QueueMarshalledCmd marshals a BackendCmd to JSON and sends it to the
// specified queue URL. Returns an error if marshalling or sending fails.
func UnmarshallQueuedCmd(record events.SQSMessage) (BackendCmd, error) {
	cmd := BackendCmd{}
	err := json.Unmarshal([]byte(record.Body), &cmd)
	return cmd, err
}

// MarshalCustomID marshals a BackendCmd to a JSON string, ensuring it
// does not exceed 100 characters, as required by the Discord API for
// CustomIDs. Returns an error if the limit is exceeded.
func MarshalCustomID(cmd BackendCmd) (string, error) {
	bodyBytes, err := json.Marshal(cmd)
	if err != nil {
		return "", fmt.Errorf("json.Marshal / %w", err)
	}
	if len(bodyBytes) > 100 {
		return "", fmt.Errorf("generated CustomID is longer than 100 characters which breaks Discord API")
	}
	return string(bodyBytes[:]), err
}

// UnmarshallCustomID unmarshals a Discord CustomID back into a BackendCmd
func UnmarshallCustomID(customID string) (BackendCmd, error) {
	cmd := BackendCmd{}
	err := json.Unmarshal([]byte(customID), &cmd)
	return cmd, err
}
