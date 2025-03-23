package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/caarlos0/env"
	"go.uber.org/zap"
)

//===== Section: BotEnv

type DiscordSecrets struct {
	Pkey         string `json:"pkey"`
	Token        string `json:"token"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type Lsdc2Stack struct {
	AwsRegion        string   `env:"AWS_REGION"`
	DiscordParam     string   `env:"DISCORD_PARAM"`
	QueueUrl         string   `env:"BOT_QUEUE_URL"`
	Vpc              string   `env:"VPC"`
	Subnets          []string `env:"SUBNETS" envSeparator:";"`
	Cluster          string   `env:"CLUSTER"`
	LogGroup         string   `env:"LOG_GROUP"`
	Bucket           string   `env:"SAVEGAME_BUCKET"`
	SpecTable        string   `env:"SPEC_TABLE"`
	GuildTable       string   `env:"GUILD_TABLE"`
	InstanceTable    string   `env:"INSTANCE_TABLE"`
	ExecutionRoleArn string   `env:"EXECUTION_ROLE_ARN"`
	TaskRoleArn      string   `env:"TASK_ROLE_ARN"`
}

type BotEnv struct {
	Lsdc2Stack
	DiscordSecrets
	Logger *zap.Logger
}

func InitBot() (BotEnv, error) {
	bot := BotEnv{}

	if os.Getenv("DEBUG") != "" {
		bot.Logger, _ = zap.NewDevelopment()
	} else {
		bot.Logger, _ = zap.NewProduction()
	}
	defer bot.Logger.Sync()

	err := env.Parse(&bot.Lsdc2Stack)
	if err != nil {
		return bot, fmt.Errorf("env.Parse / %w", err)
	}

	discordSecret, err := GetParameter(bot.DiscordParam)
	if err != nil {
		return bot, fmt.Errorf("GetParameter / %w", err)
	}

	err = json.Unmarshal([]byte(discordSecret), &bot.DiscordSecrets)
	if err != nil {
		return bot, fmt.Errorf("json.Unmarshal / %w", err)
	}

	return bot, nil
}

//===== Section: ServerSpec

// TODO: implement a PortAndProtocol struct in case ECS allow for udp+tcp port forward
// type PortAndProtocol struct {
// 	Port     int32  `json:"port"`
// 	Protocol string `json:"protocol"`
// }

type ServerSpec struct {
	Name          string            `json:"key" dynamodbav:"key"`
	Image         string            `json:"image" dynamodbav:"image"`
	Cpu           string            `json:"cpu" dynamodbav:"cpu"`
	Memory        string            `json:"memory" dynamodbav:"memory"`
	Storage       int32             `json:"storage" dynamodbav:"storage"`
	PortMap       map[string]string `json:"portMap" dynamodbav:"portMap"`
	EnvMap        map[string]string `json:"envMap" dynamodbav:"envMap"`
	EnvParamMap   map[string]string `json:"EnvParamMap" dynamodbav:"EnvParamMap"` // FIXME: un-capitalised for consistency
	SecurityGroup string            `json:"securityGroup" dynamodbav:"securityGroup"`
	ServerCount   int               `json:"severCount" dynamodbav:"severCount"`
}

// MissingField returns a list of required ServerSpec fields
func (s ServerSpec) MissingField() []string {
	missingFields := []string{}
	if s.Name == "" {
		missingFields = append(missingFields, "name")
	}
	if s.Image == "" {
		missingFields = append(missingFields, "image")
	}
	if s.Cpu == "" {
		missingFields = append(missingFields, "cpu")
	}
	if s.Memory == "" {
		missingFields = append(missingFields, "memory")
	}
	if len(s.PortMap) == 0 {
		missingFields = append(missingFields, "portMap")
	}

	return missingFields
}

// Custom JSON unmarshaler for the ServerSpec type.
func (s *ServerSpec) UnmarshalJSON(data []byte) error {
	type Alias ServerSpec
	aux := &struct {
		Storage *int32 `json:"storage"`
		*Alias
	}{
		Alias: (*Alias)(s),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if aux.Storage == nil {
		s.Storage = 21 // Minimal default value for Storage
	} else {
		s.Storage = *aux.Storage
	}

	return nil
}

// OpenPorts returns a string representation of ServerSpec ports
func (s ServerSpec) OpenPorts() []string {
	keys := make([]string, len(s.PortMap))

	idx := 0
	for k := range s.PortMap {
		keys[idx] = k
		idx++
	}
	return keys
}

// AwsEnvSpec returns a []*ecs.KeyValuePair representation of
// the ServerSpec environment variables
func (s *ServerSpec) AwsEnvSpec() []ecsTypes.KeyValuePair {
	envArray := make([]ecsTypes.KeyValuePair, len(s.EnvMap))
	idx := 0
	for k, v := range s.EnvMap {
		envArray[idx] = ecsTypes.KeyValuePair{Name: aws.String(k), Value: aws.String(v)}
		idx = idx + 1
	}
	return envArray
}

// AwsPortSpec returns a []*ecs.PortMapping representation of
// the ServerSpec ports
func (s *ServerSpec) AwsPortSpec() []ecsTypes.PortMapping {
	portArray := make([]ecsTypes.PortMapping, len(s.PortMap))
	idx := 0
	for portStr, protocolStr := range s.PortMap {
		var protocol ecsTypes.TransportProtocol
		if protocolStr == "udp" {
			protocol = ecsTypes.TransportProtocolUdp
		} else {
			protocol = ecsTypes.TransportProtocolTcp
		}
		port, _ := strconv.ParseInt(portStr, 10, 64)
		portArray[idx] = ecsTypes.PortMapping{
			ContainerPort: aws.Int32(int32(port)),
			HostPort:      aws.Int32(int32(port)),
			Protocol:      protocol,
		}
		idx = idx + 1
	}
	return portArray
}

// AwsIpPermissionSpec returns a []*ec2.IpPermission representation of
// the ServerSpec ports and protocols
func (s *ServerSpec) AwsIpPermissionSpec() []ec2Types.IpPermission {
	permissions := make([]ec2Types.IpPermission, len(s.PortMap))
	idx := 0
	for portStr, protocol := range s.PortMap {
		port, _ := strconv.ParseInt(portStr, 10, 64)
		permissions[idx] = ec2Types.IpPermission{
			FromPort:   aws.Int32(int32(port)),
			ToPort:     aws.Int32(int32(port)),
			IpProtocol: aws.String(protocol),
			IpRanges: []ec2Types.IpRange{
				{CidrIp: aws.String("0.0.0.0/0")},
			},
		}
		idx = idx + 1
	}
	return permissions
}

//===== Section: GuildConf

type GuildConf struct {
	GuildID           string `json:"key" dynamodbav:"key"`
	ChannelCategoryID string `json:"channelCategory" dynamodbav:"channelCategory"`
	AdminChannelID    string `json:"adminChannel" dynamodbav:"adminChannel"`
	WelcomeChannelID  string `json:"welcomeChannel" dynamodbav:"welcomeChannel"`
	AdminRoleID       string `json:"adminRole" dynamodbav:"adminRole"`
	UserRoleID        string `json:"userRole" dynamodbav:"userRole"`
}

//===== Section: ServerInstance

type ServerInstance struct {
	ChannelID  string `json:"key" dynamodbav:"key"`
	GuildID    string `json:"guildID" dynamodbav:"guildID"`
	Name       string `json:"name" dynamodbav:"name"`
	SpecName   string `json:"specName" dynamodbav:"specName"`
	TaskFamily string `json:"taskFamily" dynamodbav:"taskFamily"`
	TaskArn    string `json:"taskArn" dynamodbav:"taskArn"`
	ThreadID   string `json:"threadID" dynamodbav:"threadID"`
}

//===== Section: ECS task status

const (
	TaskStopped = iota
	TaskStopping
	TaskStarting
	TaskRunning
)

// GetTaskStatus return a simplified ECS task lifecycle
// TaskStarting > TaskRunning > TaskStopping > TaskStopped
func GetTaskStatus(task *ecsTypes.Task) int {
	if (task == nil) || *task.LastStatus == string(ecsTypes.DesiredStatusStopped) {
		return TaskStopped
	} else if *task.DesiredStatus == string(ecsTypes.DesiredStatusStopped) {
		return TaskStopping
	} else if *task.LastStatus == string(ecsTypes.DesiredStatusRunning) {
		return TaskRunning
	} else {
		return TaskStarting
	}
}
