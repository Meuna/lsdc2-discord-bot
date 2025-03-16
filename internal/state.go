package internal

import (
	"encoding/json"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/caarlos0/env"
	"go.uber.org/zap"
)

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
		return bot, err
	}

	discordSecret, err := GetParameter(bot.DiscordParam)
	if err != nil {
		return bot, err
	}

	err = json.Unmarshal([]byte(discordSecret), &bot.DiscordSecrets)
	if err != nil {
		return bot, err
	}

	return bot, nil
}

type ServerSpec struct {
	Name          string            `json:"key"`
	Image         string            `json:"image"`
	Cpu           string            `json:"cpu"`
	Memory        string            `json:"memory"`
	Storage       int64             `json:"storage"`
	PortMap       map[string]string `json:"portMap"`
	EnvMap        map[string]string `json:"envMap"`
	EnvParamMap   map[string]string `json:"EnvParamMap"`
	SecurityGroup string            `json:"securityGroup"`
	ServerCount   int               `json:"severCount"`
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
		Storage *int64 `json:"storage"`
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
func (s *ServerSpec) AwsEnvSpec() []*ecs.KeyValuePair {
	envArray := make([]*ecs.KeyValuePair, len(s.EnvMap))
	idx := 0
	for k, v := range s.EnvMap {
		envArray[idx] = &ecs.KeyValuePair{Name: aws.String(k), Value: aws.String(v)}
		idx = idx + 1
	}
	return envArray
}

// AwsPortSpec returns a []*ecs.PortMapping representation of
// the ServerSpec ports
func (s *ServerSpec) AwsPortSpec() []*ecs.PortMapping {
	portArray := make([]*ecs.PortMapping, len(s.PortMap))
	idx := 0
	for portStr, protocol := range s.PortMap {
		port, _ := strconv.ParseInt(portStr, 10, 64)
		portArray[idx] = &ecs.PortMapping{
			ContainerPort: aws.Int64(port),
			HostPort:      aws.Int64(port),
			Protocol:      aws.String(protocol),
		}
		idx = idx + 1
	}
	return portArray
}

// AwsIpPermissionSpec returns a []*ec2.IpPermission representation of
// the ServerSpec ports and protocols
func (s *ServerSpec) AwsIpPermissionSpec() []*ec2.IpPermission {
	permissions := make([]*ec2.IpPermission, len(s.PortMap))
	idx := 0
	for portStr, protocol := range s.PortMap {
		port, _ := strconv.ParseInt(portStr, 10, 64)
		permissions[idx] = &ec2.IpPermission{
			FromPort:   aws.Int64(port),
			ToPort:     aws.Int64(port),
			IpProtocol: aws.String(protocol),
			IpRanges: []*ec2.IpRange{
				{CidrIp: aws.String("0.0.0.0/0")},
			},
		}
		idx = idx + 1
	}
	return permissions
}

type GuildConf struct {
	GuildID           string `json:"key"`
	ChannelCategoryID string `json:"channelCategory"`
	AdminChannelID    string `json:"adminChannel"`
	WelcomeChannelID  string `json:"welcomeChannel"`
	AdminRoleID       string `json:"adminRole"`
	UserRoleID        string `json:"userRole"`
}

type ServerInstance struct {
	ChannelID  string `json:"key"`
	GuildID    string `json:"guildID"`
	Name       string `json:"name"`
	SpecName   string `json:"specName"`
	TaskFamily string `json:"taskFamily"`
	TaskArn    string `json:"taskArn"`
	ThreadID   string `json:"threadID"`
}

const (
	TaskStopped = iota
	TaskStopping
	TaskStarting
	TaskRunning
)

// GetTaskStatus return a simplified ECS task lifecycle
// TaskStarting > TaskRunning > TaskStopping > TaskStopped
func GetTaskStatus(task *ecs.Task) int {
	if (task == nil) || *task.LastStatus == ecs.DesiredStatusStopped {
		return TaskStopped
	} else if *task.DesiredStatus == ecs.DesiredStatusStopped {
		return TaskStopping
	} else if *task.LastStatus == ecs.DesiredStatusRunning {
		return TaskRunning
	} else {
		return TaskStarting
	}
}
