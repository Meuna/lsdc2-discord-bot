package internal

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sort"

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
	ServerSpecTable     string   `env:"SERVER_SPEC_TABLE"`
	EngineTierTable     string   `env:"ENGINE_TIER_TABLE"`
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

//===== Section: EngineTier

type EngineTier struct {
	Name          string                  `json:"name" dynamodbav:"key"`
	Cpu           string                  `json:"cpu"`
	Memory        string                  `json:"memory"`
	InstanceTypes []ec2Types.InstanceType `json:"instanceTypes"`
}

func (st EngineTier) MissingField() []string {
	missingFields := []string{}
	if st.Name == "" {
		missingFields = append(missingFields, "name")
	}
	if st.Cpu == "" {
		missingFields = append(missingFields, "cpu")
	}
	if st.Memory == "" {
		missingFields = append(missingFields, "memory")
	}
	if len(st.InstanceTypes) == 0 {
		missingFields = append(missingFields, "instanceTypes")
	}
	return missingFields
}

//===== Section: ServerSpec

type EngineType string

const (
	EcsEngineType EngineType = "ecs"
	Ec2EngineType EngineType = "ec2"
)

// TODO: add interfaces to remove all the type switches
type Engine interface {
	MissingField() []string
	ApplyEngineTier(EngineTier)
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

func (e *EcsEngine) ApplyEngineTier(tier EngineTier) {
	if tier.Cpu != "" {
		e.Cpu = tier.Cpu
	}
	if tier.Memory != "" {
		e.Memory = tier.Memory
	}
}

type Ec2Engine struct {
	Ami           string                  `json:"ami"`
	InstanceTypes []ec2Types.InstanceType `json:"instanceTypes"`
	Iops          int32                   `json:"iops"`
	Throughput    int32                   `json:"throughput"`
	Fastboot      bool                    `json:"fastboot"`
}

// MissingField returns a list of required ServerSpec fields
func (e Ec2Engine) MissingField() []string {
	missingFields := []string{}
	if e.Ami == "" {
		missingFields = append(missingFields, "image")
	}
	if len(e.InstanceTypes) == 0 {
		missingFields = append(missingFields, "instanceTypes")
	}
	return missingFields
}

func (e *Ec2Engine) ApplyEngineTier(tier EngineTier) {
	if len(tier.InstanceTypes) > 0 {
		e.InstanceTypes = tier.InstanceTypes
	}
}

type ServerSpec struct {
	Name          string             `json:"name" dynamodbav:"key"`
	Ingress       map[string][]int32 `json:"ingress"`
	EnvMap        map[string]string  `json:"envMap"`
	EnvParamMap   map[string]string  `json:"envParamMap"`
	SecurityGroup string             `json:"securityGroup"`
	ServerCount   int                `json:"severCount"`
	EngineType    EngineType         `json:"engineType"`
	Engine        Engine             `json:"engine"`
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
	if len(s.Ingress) == 0 {
		missingFields = append(missingFields, "ingress")
	}

	if s.Engine != nil {
		missingFields = append(missingFields, s.Engine.MissingField()...)
	}

	return missingFields
}

// OpenPorts returns a string representation of ServerSpec ports
func (s ServerSpec) DescribeIngress() []string {
	ingressCount := 0
	for _, ports := range s.Ingress {
		ingressCount = ingressCount + len(ports)
	}

	describedIngress := make([]string, ingressCount)
	idx := 0
	for proto, ports := range s.Ingress {
		for _, port := range ports {
			describedIngress[idx] = fmt.Sprintf("%d/%s", port, proto)
		}
		idx++
	}
	sort.Strings(describedIngress)

	return describedIngress
}

// AwsPortMapping returns a []*ecs.PortMapping representation of
// the ServerSpec ports
func (s ServerSpec) AwsPortMapping() []ecsTypes.PortMapping {
	ingressCount := 0
	for _, ports := range s.Ingress {
		ingressCount = ingressCount + len(ports)
	}

	portMapping := make([]ecsTypes.PortMapping, ingressCount)
	idx := 0
	for proto, ports := range s.Ingress {
		for _, port := range ports {
			var protocol ecsTypes.TransportProtocol
			if proto == "udp" {
				protocol = ecsTypes.TransportProtocolUdp
			} else {
				protocol = ecsTypes.TransportProtocolTcp
			}
			portMapping[idx] = ecsTypes.PortMapping{
				ContainerPort: aws.Int32(port),
				HostPort:      aws.Int32(port),
				Protocol:      protocol,
			}
			idx = idx + 1
		}
	}
	return portMapping
}

// AwsIpPermissionSpec returns a []*ec2.IpPermission representation of
// the ServerSpec ports and protocols
func (s ServerSpec) AwsIpPermissionSpec() []ec2Types.IpPermission {
	ingressCount := 0
	for _, ports := range s.Ingress {
		ingressCount = ingressCount + len(ports)
	}

	permissions := make([]ec2Types.IpPermission, ingressCount)
	idx := 0
	for proto, ports := range s.Ingress {
		for _, port := range ports {
			permissions[idx] = ec2Types.IpPermission{
				FromPort:   aws.Int32(port),
				ToPort:     aws.Int32(port),
				IpProtocol: aws.String(proto),
				IpRanges: []ec2Types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0")},
				},
			}
			idx = idx + 1
		}
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

func (srv Server) StartInstance(bot BotEnv, srvTier EngineTier) (Instance, error) {
	// Get the game spec
	spec := ServerSpec{}
	if err := DynamodbGetItem(bot.ServerSpecTable, srv.SpecName, &spec); err != nil {
		return Instance{}, fmt.Errorf("StartTask / %w", err)
	}

	// Apply the optional engine tier
	if srvTier.Name != "" {
		spec.Engine.ApplyEngineTier(srvTier)
	}

	// Prepare instance entry
	inst := Instance{
		EngineType:      spec.EngineType,
		SpecName:        srv.SpecName,
		ServerName:      srv.Name,
		ServerChannelID: srv.ChannelID,
		ServerGuildID:   srv.GuildID,
		OpenPorts:       fmt.Sprintf("%s", spec.DescribeIngress()),
	}

	// Build the instance environment
	instEnvMap := map[string]string{
		"LSDC2_QUEUE_URL": bot.QueueUrl,
		"LSDC2_LOG_GROUP": bot.LogGroup,
		"LSDC2_BUCKET":    bot.Bucket,
		"LSDC2_SERVER":    srv.Name,
		"DEBUG":           os.Getenv("DEBUG"),
	}
	if spec.EnvMap != nil {
		maps.Copy(instEnvMap, spec.EnvMap)
	}
	maps.Copy(instEnvMap, srv.EnvMap)

	if spec.EngineType == EcsEngineType {
		if err := RegisterTaskFamily(bot.Lsdc2Stack, spec, instEnvMap, srv.Name); err != nil {
			return Instance{}, fmt.Errorf("RegisterTask / %w", err)
		}
		taskArn, err := StartEcsTask(bot.Lsdc2Stack, spec, srv.Name)
		if err != nil {
			return Instance{}, fmt.Errorf("StartEcsTask / %w", err)
		}
		inst.EngineID = taskArn
	} else {
		instanceID, err := StartEc2VM(bot.Lsdc2Stack, spec, instEnvMap)
		if err != nil {
			return Instance{}, fmt.Errorf("StartEc2Instance / %w", err)
		}
		inst.EngineID = instanceID
	}
	return inst, nil
}

//===== Section: Instace

type Instance struct {
	EngineID        string `dynamodbav:"key"`
	EngineType      EngineType
	ThreadID        string
	SpecName        string
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
		if err := inst.DeregisterTaskFamily(bot); err != nil {
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
	return DeregisterTaskFamily(bot.Lsdc2Stack, inst.ServerName)
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
