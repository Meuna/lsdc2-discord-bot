package internal

import (
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/caarlos0/env"
)

type DiscordSecrets struct {
	Pkey  string `env:"DISCORD_PKEY"`
	Token string `env:"DISCORD_TOKEN"`
}

type Lsdc2Stack struct {
	QueueUrl         string   `env:"BOT_QUEUE_URL"`
	Vpc              string   `env:"VPC"`
	Subnets          []string `env:"SUBNETS" envSeparator:";"`
	Cluster          string   `env:"CLUSTER"`
	LogGroup         string   `env:"LOG_GROUP"`
	SaveGameBucket   string   `env:"SAVEGAME_BUCKET"`
	SpecTable        string   `env:"SPEC_TABLE"`
	InstanceTable    string   `env:"INSTANCE_TABLE"`
	ExecutionRoleArn string   `env:"EXECUTION_ROLE_ARN"`
	TaskRoleArn      string   `env:"TASK_ROLE_ARN"`
}

type BotEnv struct {
	Lsdc2Stack
	DiscordSecrets
}

func ParseEnv() (BotEnv, error) {
	bot := BotEnv{}

	err := env.Parse(&bot.Lsdc2Stack)
	if err != nil {
		return bot, err
	}

	err = env.Parse(&bot.DiscordSecrets)
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
	PortMap       map[string]string `json:"portMap"`
	EnvMap        map[string]string `json:"envMap"`
	EnvParamMap   map[string]string `json:"EnvParamMap"`
	SecurityGroup string            `json:"securityGroup"`
	ServerCount   int               `json:"severCount"`
}

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

func (s ServerSpec) OpenPorts() []string {
	keys := make([]string, len(s.PortMap))

	idx := 0
	for k := range s.PortMap {
		keys[idx] = k
		idx++
	}
	return keys
}

func (s *ServerSpec) AwsEnvSpec() []*ecs.KeyValuePair {
	envArray := make([]*ecs.KeyValuePair, len(s.EnvMap))
	idx := 0
	for k, v := range s.EnvMap {
		envArray[idx] = &ecs.KeyValuePair{Name: aws.String(k), Value: aws.String(v)}
		idx = idx + 1
	}
	return envArray
}

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

type ServerInstance struct {
	ChannelID     string `json:"key"`
	Name          string `json:"name"`
	SpecName      string `json:"specName"`
	TaskFamily    string `json:"taskFamily"`
	SecurityGroup string `json:"securityGroup"`
	TaskArn       string `json:"taskArn"`
}

const (
	TaskStopped = iota
	TaskStopping
	TaskProvisioning
	TaskContainerStopping
	TaskContainerProvisioning
	TaskRunning
)

func GetTaskStatus(task *ecs.Task) int {
	if (task == nil) || *task.LastStatus == "STOPPED" {
		return TaskStopped
	}
	offlineStatus := []string{"DEACTIVATING", "STOPING", "DEPROVISIONING"}
	if Contains(offlineStatus, *task.LastStatus) {
		return TaskStopping
	}
	provisioningStatus := []string{"PROVISIONING", "PENDING", "ACTIVATING"}
	if Contains(provisioningStatus, *task.LastStatus) {
		return TaskProvisioning
	}
	// From here, we know that task.LastStatus="RUNNING"
	offlineStatus = []string{"REGISTRATION_FAILED", "INACTIVE", "DEREGISTERING", "DRAINING"}
	if len(task.Containers) == 0 || Contains(offlineStatus, *task.Containers[0].LastStatus) {
		return TaskContainerStopping
	}
	if *task.Containers[0].LastStatus == "REGISTERING" {
		return TaskContainerProvisioning
	}
	// We take a last step to check that the task is not heading offline
	if *task.DesiredStatus == "STOPPED" {
		return TaskContainerStopping
	}
	return TaskRunning
}
