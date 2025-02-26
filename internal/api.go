package internal

import (
	"encoding/json"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

const (
	RegisterGameAPI = "register-game"
	WelcomeAPI      = "welcome-guild"
	GoodbyeAPI      = "goodbye-guild"
	SpinupAPI       = "spinup"
	DestroyAPI      = "destroy"
	InviteAPI       = "invite"
	KickAPI         = "kick"
	StartAPI        = "start"
	StopAPI         = "stop"
	StatusAPI       = "status"
	DownloadAPI     = "download"
	UploadAPI       = "upload"
)

var (
	OwnerCmd      = []string{RegisterGameAPI, WelcomeAPI, GoodbyeAPI}
	AdminCmd      = []string{SpinupAPI, DestroyAPI, InviteAPI, KickAPI}
	InviteKickCmd = []string{InviteAPI, KickAPI}
	UserCmd       = []string{StartAPI, StopAPI, StatusAPI, DownloadAPI, UploadAPI}
)

const (
	RegisterGameAPISpecUrlOpt   string = "spec-url"
	RegisterGameAPIOverwriteOpt string = "overwrite"
)

type BackendCmd struct {
	Args  interface{}
	AppID string `json:",omitempty"`
	Token string `json:",omitempty"`
}

func (cmd BackendCmd) Action() string {
	switch cmd.Args.(type) {
	case *RegisterGameArgs, RegisterGameArgs:
		return RegisterGameAPI
	case *WelcomeArgs, WelcomeArgs:
		return WelcomeAPI
	case *GoodbyeArgs, GoodbyeArgs:
		return GoodbyeAPI
	case *SpinupArgs, SpinupArgs:
		return SpinupAPI
	case *DestroyArgs, DestroyArgs:
		return DestroyAPI
	case *InviteArgs, InviteArgs:
		return InviteAPI
	case *KickArgs, KickArgs:
		return KickAPI
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
	case WelcomeAPI:
		cmd.Args = &WelcomeArgs{}
	case GoodbyeAPI:
		cmd.Args = &GoodbyeArgs{}
	case SpinupAPI:
		cmd.Args = &SpinupArgs{}
	case DestroyAPI:
		cmd.Args = &DestroyArgs{}
	case InviteAPI:
		cmd.Args = &InviteArgs{}
	case KickAPI:
		cmd.Args = &KickArgs{}
	default:
		return fmt.Errorf("unknown command: %w", tmp.Action)
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
