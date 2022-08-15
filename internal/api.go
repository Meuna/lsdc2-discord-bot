package internal

import (
	"encoding/json"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

const (
	RegisterGameAPI = "register-game"
	BootstrapAPI    = "bootstrap"
	SpinupAPI       = "spinup"
	DestroyAPI      = "destroy"
	StartAPI        = "start"
	StopAPI         = "stop"
	StatusAPI       = "status"
	DownloadAPI     = "download"
	UploadAPI       = "upload"
)

var (
	OwnerCmd = []string{RegisterGameAPI, BootstrapAPI}
	AdminCmd = []string{SpinupAPI, DestroyAPI}
	UserCmd  = []string{StartAPI, StopAPI, StatusAPI, DownloadAPI, UploadAPI}
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
