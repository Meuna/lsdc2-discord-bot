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

// Structure used to communicate bot intent between the frontend and the
// backend, but also between frontend roundtrips (modals and message
// components). The Args and Api field works together, with the Api field
// set by the Args Type upon JSON marshalling and the Args Type set by the
// Api field upon unmarshalling.
type BackendCmd struct {
	AppID string `json:",omitempty"`
	Token string `json:",omitempty"`
	Api   string
	Args  any
}

// Custom JSON unmarshaler for the BackendCmd type. It first unmarshals
// the JSON into a temporary structure to handle the dynamic nature of
// the Args field based on the Api field. Depending on the value of Api,
// it initializes the appropriate Args structure and then unmarshals the
// Args field into this structure.
func (cmd *BackendCmd) UnmarshalJSON(src []byte) error {
	type backendCmd BackendCmd
	var tmp struct {
		backendCmd
		Args json.RawMessage
	}
	err := json.Unmarshal(src, &tmp)
	if err != nil {
		return err
	}

	*cmd = BackendCmd(tmp.backendCmd)

	switch tmp.Api {
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
		return fmt.Errorf("unknown command: %s", tmp.Api)
	}

	return json.Unmarshal(tmp.Args, cmd.Args)
}

// Custom JSON marshaler for the BackendCmd type. It sets the Api field
// based on the type of Args and then marshals the BackendCmd to JSON.
// If the Args type is not recognized, it returns an error.
func (cmd BackendCmd) MarshalJSON() ([]byte, error) {
	type backendCmd BackendCmd

	switch cmd.Args.(type) {
	case *RegisterGameArgs, RegisterGameArgs:
		cmd.Api = RegisterGameAPI
	case *WelcomeArgs, WelcomeArgs:
		cmd.Api = WelcomeAPI
	case *GoodbyeArgs, GoodbyeArgs:
		cmd.Api = GoodbyeAPI
	case *SpinupArgs, SpinupArgs:
		cmd.Api = SpinupAPI
	case *DestroyArgs, DestroyArgs:
		cmd.Api = DestroyAPI
	case *InviteArgs, InviteArgs:
		cmd.Api = InviteAPI
	case *KickArgs, KickArgs:
		cmd.Api = KickAPI
	default:
		return nil, fmt.Errorf("incompatible BackendCmd Args type %T", cmd.Args)
	}

	return json.Marshal(backendCmd(cmd))
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
