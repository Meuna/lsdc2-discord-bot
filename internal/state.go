package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	dynamodbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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
	AwsRegion           string   `env:"AWS_REGION"`
	DiscordParam        string   `env:"DISCORD_PARAM"`
	QueueUrl            string   `env:"BOT_QUEUE_URL"`
	Vpc                 string   `env:"VPC"`
	Subnets             []string `env:"SUBNETS" envSeparator:";"`
	LogGroup            string   `env:"LOG_GROUP"`
	Bucket              string   `env:"SAVEGAME_BUCKET"`
	SpecTable           string   `env:"SPEC_TABLE"`
	GuildTable          string   `env:"GUILD_TABLE"`
	ServerTable         string   `env:"SERVER_TABLE"`
	InstanceTable       string   `env:"INSTANCE_TABLE"`
	EcsClusterName      string   `env:"ECS_CLUSTER_NAME"`
	EcsExecutionRoleArn string   `env:"ECS_EXECUTION_ROLE_ARN"`
	EcsTaskRoleArn      string   `env:"ECS_TASK_ROLE_ARN"`
	Ec2VMProfileArn     string   `env:"EC2_VM_PROFILE_ARN"`
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

type EngineType string

const (
	EcsEngineType EngineType = "ecs"
	Ec2EngineType EngineType = "ec2"
)

type Engine interface {
	MissingField() []string
}

type EcsEngine struct {
	Image   string `json:"image"`
	Cpu     string `json:"cpu"`
	Memory  string `json:"memory"`
	Storage int32  `json:"storage"`
}

// MissingField returns a list of required ServerSpec fields
func (e EcsEngine) MissingField() []string {
	missingFields := []string{}
	if e.Image == "" {
		missingFields = append(missingFields, "image")
	}
	if e.Cpu == "" {
		missingFields = append(missingFields, "cpu")
	}
	if e.Memory == "" {
		missingFields = append(missingFields, "memory")
	}

	return missingFields
}

type Ec2Engine struct {
	Ami          string `json:"ami"`
	InstanceType string `json:"instanceType"`
	Storage      int32  `json:"storage"`
}

// MissingField returns a list of required ServerSpec fields
func (e Ec2Engine) MissingField() []string {
	missingFields := []string{}
	if e.Ami == "" {
		missingFields = append(missingFields, "image")
	}
	if e.InstanceType == "" {
		missingFields = append(missingFields, "instanceType")
	}
	return missingFields
}

// TODO: implement a PortAndProtocol struct in case ECS allow for udp+tcp port forward
// type PortAndProtocol struct {
// 	Port     int32  `json:"port"`
// 	Protocol string `json:"protocol"`
// }

type ServerSpec struct {
	Name          string            `json:"name" dynamodbav:"key"`
	PortMap       map[string]string `json:"portMap"`
	EnvMap        map[string]string `json:"envMap"`
	EnvParamMap   map[string]string `json:"envParamMap"`
	SecurityGroup string            `json:"securityGroup"`
	ServerCount   int               `json:"severCount"`
	EngineType    EngineType        `json:"engineType"`
	Engine        Engine            `json:"engine"`
}

// Custom JSON unmarshaler for the ServerSpec type
func (s *ServerSpec) UnmarshalJSON(data []byte) error {
	type Alias ServerSpec
	aux := &struct {
		Engine json.RawMessage `json:"engine"`
		*Alias
	}{
		Alias: (*Alias)(s),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	switch aux.EngineType {
	case EcsEngineType:
		s.Engine = &EcsEngine{}
	case Ec2EngineType:
		s.Engine = &Ec2Engine{}
	default:
		return fmt.Errorf("unknown engine type: %s", aux.EngineType)
	}

	return json.Unmarshal(aux.Engine, s.Engine)
}

// Custom DynamoDB unmarshaler for the ServerSpec type
func (s *ServerSpec) UnmarshalDynamoDBAttributeValue(av dynamodbTypes.AttributeValue) error {
	type Alias ServerSpec
	aux := &struct {
		Engine any
		*Alias
	}{
		Alias: (*Alias)(s),
	}

	if err := attributevalue.Unmarshal(av, &aux); err != nil {
		return err
	}

	switch aux.EngineType {
	case EcsEngineType:
		s.Engine = &EcsEngine{}
	case Ec2EngineType:
		s.Engine = &Ec2Engine{}
	default:
		// Silently return to handle empty AttributeValue
		return nil
	}

	avM := av.(*dynamodbTypes.AttributeValueMemberM)
	engineAv, ok := avM.Value["Engine"]
	if !ok {
		// Silently return to handle empty AttributeValue
		return nil
	}

	if err := attributevalue.Unmarshal(engineAv, &s.Engine); err != nil {
		return err
	}

	return nil
}

// MissingField returns a list of required ServerSpec fields
func (s ServerSpec) MissingField() []string {
	missingFields := []string{}
	if s.Name == "" {
		missingFields = append(missingFields, "name")
	}
	if len(s.PortMap) == 0 {
		missingFields = append(missingFields, "portMap")
	}

	if s.Engine != nil {
		missingFields = append(missingFields, s.Engine.MissingField()...)
	}

	return missingFields
}

// OpenPorts returns a string representation of ServerSpec ports
func (s ServerSpec) OpenPorts() []string {
	keys := make([]string, len(s.PortMap))

	idx := 0
	for k := range s.PortMap {
		keys[idx] = k
		idx++
	}
	sort.Strings(keys)
	return keys
}

// AwsEnvSpec returns a []*ecs.KeyValuePair representation of
// the ServerSpec environment variables
func (s ServerSpec) AwsEnvSpec() []ecsTypes.KeyValuePair {
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
func (s ServerSpec) AwsPortSpec() []ecsTypes.PortMapping {
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
func (s ServerSpec) AwsIpPermissionSpec() []ec2Types.IpPermission {
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
	GuildID           string `dynamodbav:"key"`
	ChannelCategoryID string
	AdminChannelID    string
	WelcomeChannelID  string
	AdminRoleID       string
	UserRoleID        string
}

//===== Section: Server

type Server struct {
	ChannelID string `dynamodbav:"key"`
	GuildID   string
	Name      string
	SpecName  string
	EnvMap    map[string]string
}

func (srv Server) StartInstance(bot BotEnv) (Instance, error) {
	// Get the game spec
	spec := ServerSpec{}
	if err := DynamodbGetItem(bot.SpecTable, srv.SpecName, &spec); err != nil {
		return Instance{}, fmt.Errorf("StartTask / %w", err)
	}

	inst := Instance{
		EngineType:      spec.EngineType,
		ServerName:      srv.Name,
		ServerChannelID: srv.ChannelID,
		ServerGuildID:   srv.GuildID,
		OpenPorts:       fmt.Sprintf("%s", spec.OpenPorts()),
	}

	if spec.EngineType == EcsEngineType {
		taskFamily := taskFamily(srv.GuildID, srv.Name)
		if err := RegisterTaskFamily(bot.Lsdc2Stack, spec, srv.EnvMap, taskFamily); err != nil {
			return Instance{}, fmt.Errorf("RegisterTask / %w", err)
		}
		taskArn, err := StartEcsTask(bot.Lsdc2Stack, spec, taskFamily)
		if err != nil {
			return Instance{}, fmt.Errorf("StartEcsTask / %w", err)
		}
		inst.EngineID = taskArn
	} else {
		instanceID, err := StartEc2VM(bot.Lsdc2Stack, spec, srv.EnvMap)
		if err != nil {
			return Instance{}, fmt.Errorf("StartEc2Instance / %w", err)
		}
		inst.EngineID = instanceID
	}
	return inst, nil
}

func taskFamily(guildID string, serverName string) string {
	return fmt.Sprintf("lsdc2-%s-%s", guildID, serverName)
}

//===== Section: Instace

type Instance struct {
	EngineID        string `dynamodbav:"key"`
	EngineType      EngineType
	ThreadID        string
	ServerName      string
	ServerChannelID string
	ServerGuildID   string
	OpenPorts       string
}

func (inst Instance) StopInstance(bot BotEnv) error {
	if inst.EngineType == EcsEngineType {
		if err := StopEcsTask(inst.EngineID, bot.Lsdc2Stack.EcsClusterName); err != nil {
			return fmt.Errorf("StopEcsTask / %w", err)
		}
		taskFamily := taskFamily(inst.ServerGuildID, inst.ServerName)
		if err := DeregisterTaskFamily(taskFamily); err != nil {
			return fmt.Errorf("DeregisterTaskFamily / %w", err)
		}
	} else {
		err := SendCommand(inst.EngineID, "sudo systemctl stop lsdc2.service")
		if err != nil {
			return fmt.Errorf("StopEc2Instance / %w", err)
		}
	}
	return nil
}

func (inst Instance) DeregisterTaskFamily(bot BotEnv) error {
	taskFamily := taskFamily(inst.ServerGuildID, inst.ServerName)
	return DeregisterTaskFamily(taskFamily)
}

type InstanceState string

const (
	InstanceStateStopped  InstanceState = "stoped"
	InstanceStateStopping InstanceState = "stoping"
	InstanceStateStarting InstanceState = "starting"
	InstanceStateRunning  InstanceState = "running"
)

// GetState returns a simplified and unified EC2/ECS lifecycle
// TaskStarting > TaskRunning > TaskStopping > TaskStopped
func (inst Instance) GetState(ecsCluster string) (InstanceState, error) {
	var state InstanceState
	if inst.EngineType == EcsEngineType {
		task, err := DescribeTask(inst.EngineID, ecsCluster)
		if err != nil {
			return state, err
		}
		state = GetEcsTaskState(task)
	} else {
		vm, err := DescribeInstance(inst.EngineID)
		if err != nil {
			return state, err
		}
		state = GetEc2InstanceState(vm.State.Name)
	}

	return state, nil
}

// GetIP returns the IP of the instance
func (inst Instance) GetIP(ecsCluster string) (string, error) {
	if inst.EngineType == EcsEngineType {
		task, err := DescribeTask(inst.EngineID, ecsCluster)
		if err != nil {
			return "", err
		}
		return GetEcsTaskIP(task)
	} else {
		vm, err := DescribeInstance(inst.EngineID)
		if err != nil {
			return "", err
		}
		return *vm.PublicIpAddress, nil
	}
}

// GetEcsTaskState return the unified InstanceState of an ECS task
func GetEcsTaskState(task ecsTypes.Task) InstanceState {
	var state InstanceState
	if *task.LastStatus == string(ecsTypes.DesiredStatusStopped) {
		state = InstanceStateStopped
	} else if *task.DesiredStatus == string(ecsTypes.DesiredStatusStopped) {
		state = InstanceStateStopping
	} else if *task.LastStatus == string(ecsTypes.DesiredStatusRunning) {
		state = InstanceStateRunning
	} else {
		state = InstanceStateStarting
	}

	return state
}

// GetEcsTaskState return the unified InstanceState of an ECS task
func GetEc2InstanceState(stateName ec2Types.InstanceStateName) InstanceState {
	var state InstanceState
	switch stateName {
	case ec2Types.InstanceStateNameTerminated, ec2Types.InstanceStateNameStopped:
		state = InstanceStateStopped
	case ec2Types.InstanceStateNameStopping, ec2Types.InstanceStateNameShuttingDown:
		state = InstanceStateStopping
	case ec2Types.InstanceStateNameRunning:
		state = InstanceStateRunning
	default:
		state = InstanceStateStarting
	}

	return state
}
