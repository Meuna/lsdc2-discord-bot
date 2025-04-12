package internal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go/aws/session"
	s3v1 "github.com/aws/aws-sdk-go/service/s3" // FIXME: remove and use SDK v2 when it is able to presign completed multipart upload
	"github.com/aws/smithy-go"
)

//===== Section: SSM

// GetParameter retrieves the value of a parameter from AWS Systems Manager Parameter Store.
func GetParameter(name string) (string, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ssm.NewFromConfig(cfg)

	param, err := client.GetParameter(context.TODO(), &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}

	return *param.Parameter.Value, nil
}

//===== Section: Lambda

func Json200(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       msg,
		Headers: map[string]string{
			"content-type": "application/json",
		},
	}
}

func Html200(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       msg,
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
}

func Error401(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 401,
		Body:       msg,
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
}

func Error404() events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 404,
		Body:       "404: content not found",
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
}

func Error500() events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 500,
		Body:       "500: content not found",
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
}

//===== Section: SQS

func QueueMessage(queueUrl string, msg string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := sqs.NewFromConfig(cfg)

	_, err = client.SendMessage(context.TODO(), &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueUrl),
		MessageBody: aws.String(msg),
	})

	return err
}

//===== Section: DynamoDB

// DynamodbGetItem retrieves an item from the specified DynamoDB table, at
// the specified key and unmarshal it into the provided out pointer
func DynamodbGetItem(tableName string, key string, out any) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := dynamodb.NewFromConfig(cfg)

	rawOut, err := client.GetItem(context.TODO(), &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbTypes.AttributeValue{
			"key": &dynamodbTypes.AttributeValueMemberS{
				Value: key,
			},
		},
	})
	if err != nil {
		return err
	}
	return attributevalue.UnmarshalMap(rawOut.Item, out)
}

// DynamodbPutItem inserts an item into the specified DynamoDB table.
func DynamodbPutItem(tableName string, item any) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := dynamodb.NewFromConfig(cfg)

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return err
	}

	_, err = client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      av,
	})
	return err
}

// DynamodbDeleteItem deletes the item at the specified key from the
// specified DynamoDB table
func DynamodbDeleteItem(tableName string, key string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := dynamodb.NewFromConfig(cfg)

	_, err = client.DeleteItem(context.TODO(), &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbTypes.AttributeValue{
			"key": &dynamodbTypes.AttributeValueMemberS{
				Value: key,
			},
		},
	})
	return err
}

// DynamodbScan scans the specified DynamoDB table and returns a list of unmarshalled items
func DynamodbScan[T any](tableName string) ([]T, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}
	client := dynamodb.NewFromConfig(cfg)

	scanOut, err := client.Scan(context.TODO(), &dynamodb.ScanInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		return nil, err
	}

	out := make([]T, len(scanOut.Items))
	for idx, item := range scanOut.Items {
		err = attributevalue.UnmarshalMap(item, &out[idx])
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

// DynamodbScanFindFirst scans the specified DynamoDB table and returns the first item
// that matches the specified key-value pair. The item is unmarshalled into the output type.
func DynamodbScanFindFirst[T any](tableName string, key string, value string) (out T, err error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return out, err
	}
	client := dynamodb.NewFromConfig(cfg)

	paginator := dynamodb.NewScanPaginator(client, &dynamodb.ScanInput{
		TableName: aws.String(tableName),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			return out, err
		}
		for _, item := range page.Items {
			if val, ok := item[key]; ok {
				if s, ok := val.(*dynamodbTypes.AttributeValueMemberS); ok && s.Value == value {
					err = attributevalue.UnmarshalMap(item, &out)
					return out, err
				}
			}
		}
	}
	return
}

//===== Section: ECS

// Default ECS tag value
func ecsTags(tagMap map[string]string) []ecsTypes.Tag {
	tagList := make([]ecsTypes.Tag, len(tagMap)+2)
	tagList[0] = ecsTypes.Tag{Key: aws.String("lsdc2"), Value: aws.String("true")}
	tagList[1] = ecsTypes.Tag{Key: aws.String("lsdc2.src"), Value: aws.String("discord-bot")}
	idx := 2
	for key, value := range tagMap {
		tagList[idx] = ecsTypes.Tag{Key: aws.String(key), Value: aws.String(value)}
		idx = idx + 1
	}
	return tagList
}

// RegisterTaskFamily registers a new ECS task definition using the provided
// stack configuration, server specification, environment variables,
// and server name
func RegisterTaskFamily(stack Lsdc2Stack, spec ServerSpec, env map[string]string, serverName string) error {
	ecsSpec := spec.Engine.(*EcsEngine)
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ecs.NewFromConfig(cfg)

	envArray := make([]ecsTypes.KeyValuePair, len(env))
	idx := 0
	for k, v := range env {
		envArray[idx] = ecsTypes.KeyValuePair{Name: aws.String(k), Value: aws.String(v)}
		idx = idx + 1
	}

	input := &ecs.RegisterTaskDefinitionInput{
		Tags:                    ecsTags(map[string]string{"lsdc2.gamename": spec.Name}),
		Family:                  getTaskFamily(stack, serverName),
		Cpu:                     aws.String(ecsSpec.Cpu),
		Memory:                  aws.String(ecsSpec.Memory),
		NetworkMode:             ecsTypes.NetworkModeAwsvpc,
		TaskRoleArn:             aws.String(stack.EcsTaskRoleArn),
		ExecutionRoleArn:        aws.String(stack.EcsExecutionRoleArn),
		RequiresCompatibilities: []ecsTypes.Compatibility{ecsTypes.CompatibilityFargate},
		RuntimePlatform: &ecsTypes.RuntimePlatform{
			CpuArchitecture:       ecsTypes.CPUArchitectureX8664,
			OperatingSystemFamily: ecsTypes.OSFamilyLinux,
		},
		ContainerDefinitions: []ecsTypes.ContainerDefinition{
			{
				StopTimeout:  aws.Int32(120),
				Essential:    aws.Bool(true),
				Image:        aws.String(ecsSpec.Image),
				Name:         aws.String(serverName + "_container"),
				Environment:  envArray,
				PortMappings: spec.AwsPortMapping(),
				LogConfiguration: &ecsTypes.LogConfiguration{
					LogDriver: ecsTypes.LogDriverAwslogs,
					Options: map[string]string{
						"awslogs-group":         stack.LogGroup,
						"awslogs-region":        stack.AwsRegion,
						"awslogs-stream-prefix": "ecs",
					},
				},
			},
		},
	}
	if ecsSpec.Storage > 0 {
		input.EphemeralStorage = &ecsTypes.EphemeralStorage{SizeInGiB: min(max(21, ecsSpec.Storage), 200)}
	}
	_, err = client.RegisterTaskDefinition(context.TODO(), input)

	return err
}

// DeregisterTaskFamily deregisters all task definitions within the specified ECS task family
func DeregisterTaskFamily(stack Lsdc2Stack, serverName string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ecs.NewFromConfig(cfg)

	taskList, err := client.ListTaskDefinitions(context.TODO(), &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: getTaskFamily(stack, serverName),
	})
	if err != nil {
		return err
	}

	for _, def := range taskList.TaskDefinitionArns {
		_, err := client.DeregisterTaskDefinition(context.TODO(), &ecs.DeregisterTaskDefinitionInput{
			TaskDefinition: aws.String(def),
		})
		if err != nil {
			return err
		}
	}

	return err
}

// StartEcsTask starts an ECS task for the specified family and security group.
// Returns the ARN of the started task.
func StartEcsTask(stack Lsdc2Stack, spec ServerSpec, serverName string) (arn string, err error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ecs.NewFromConfig(cfg)

	subnets := make([]string, len(stack.Subnets))
	copy(subnets, stack.Subnets)

	result, err := client.RunTask(context.TODO(), &ecs.RunTaskInput{
		Tags: ecsTags(map[string]string{"lsdc2.gamename": spec.Name}),
		CapacityProviderStrategy: []ecsTypes.CapacityProviderStrategyItem{
			{CapacityProvider: aws.String("FARGATE_SPOT")},
		},
		Cluster: aws.String(stack.EcsClusterName),
		Count:   aws.Int32(1),
		NetworkConfiguration: &ecsTypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecsTypes.AwsVpcConfiguration{
				AssignPublicIp: ecsTypes.AssignPublicIpEnabled,
				SecurityGroups: []string{spec.SecurityGroup},
				Subnets:        subnets,
			},
		},
		TaskDefinition: getTaskFamily(stack, serverName),
	})
	if err != nil {
		arn = ""
		return
	}
	if len(result.Tasks) == 0 {
		arn = ""
		err = errors.New("task creation returned empty results")
		return
	}

	arn = *result.Tasks[0].TaskArn
	err = nil
	return
}

// StopEcsTask stops the specified ECS task in the sepcified cluster.
func StopEcsTask(taskArn string, ecsCluster string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ecs.NewFromConfig(cfg)

	_, err = client.StopTask(context.TODO(), &ecs.StopTaskInput{
		Cluster: aws.String(ecsCluster),
		Task:    aws.String(taskArn),
	})

	return err
}

// DescribeTask retrieves the details of the specified ECS task.
func DescribeTask(taskArn string, ecsCluster string) (ecsTypes.Task, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return ecsTypes.Task{}, err
	}
	client := ecs.NewFromConfig(cfg)

	result, err := client.DescribeTasks(context.TODO(), &ecs.DescribeTasksInput{
		Cluster: aws.String(ecsCluster),
		Tasks:   []string{taskArn},
	})
	if err != nil {
		return ecsTypes.Task{}, err
	}
	if len(result.Tasks) == 0 {
		return ecsTypes.Task{}, nil
	}
	return result.Tasks[0], nil
}

// GetEcsTaskIP retrieves the public IP address of the ECS task's ENI
func GetEcsTaskIP(task ecsTypes.Task) (string, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}

	// Get the ENI from the attachments
	if len(task.Attachments) == 0 {
		return "", errors.New("no ENI attached")
	}
	if *task.Attachments[0].Status != "ATTACHED" {
		return "", errors.New("ENI not in ATTACHED state")
	}
	var eniID string
	for _, kv := range task.Attachments[0].Details {
		if *kv.Name == "networkInterfaceId" {
			eniID = *kv.Value
			break
		}
	}

	// Then describe IP from ENI
	client := ec2.NewFromConfig(cfg)
	resultDni, err := client.DescribeNetworkInterfaces(context.TODO(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []string{eniID},
	})
	if err != nil {
		return "", err
	}
	if len(resultDni.NetworkInterfaces) == 0 {
		return "", errors.New("network interface description returned empty results")
	}
	if resultDni.NetworkInterfaces[0].Association == nil {
		return "", errors.New("network interface association is empty")
	}

	return *resultDni.NetworkInterfaces[0].Association.PublicIp, nil
}

// getTaskFamily implements the task family naming convention
func getTaskFamily(stack Lsdc2Stack, serverName string) *string {
	return aws.String(stack.EcsClusterName + "-" + serverName)
}

//===== Section: EC2

type instanceTypeAndSubnet struct {
	InstanceType ec2Types.InstanceType
	Subnet       string
	Price        float64
}

// Default EC2 tag value
func ec2Tags(resourceType ec2Types.ResourceType, tagMap map[string]string) []ec2Types.TagSpecification {

	tagList := make([]ec2Types.Tag, len(tagMap)+2)
	tagList[0] = ec2Types.Tag{Key: aws.String("lsdc2"), Value: aws.String("true")}
	tagList[1] = ec2Types.Tag{Key: aws.String("lsdc2.src"), Value: aws.String("discord-bot")}
	idx := 2
	for key, value := range tagMap {
		tagList[idx] = ec2Types.Tag{Key: aws.String(key), Value: aws.String(value)}
		idx = idx + 1
	}

	return []ec2Types.TagSpecification{
		{
			ResourceType: resourceType,
			Tags:         tagList,
		},
	}
}

// StartEc2VM launches a new EC2 instance based on the provided server specification.
// Returns the instance ID of the started instance.
func StartEc2VM(stack Lsdc2Stack, spec ServerSpec, env map[string]string) (string, error) {
	ec2Spec, ok := spec.Engine.(*Ec2Engine)
	if !ok {
		return "", errors.New("engine spec is not an EC2 engine")
	}

	// Get AMI ID from AMI name
	amiID, err := GetAmiID(ec2Spec.Ami)
	if err != nil {
		return "", err
	}

	// Build user data script
	var builder strings.Builder
	builder.WriteString("#!/bin/bash\n")
	builder.WriteString("cat << EOF >> /lsdc2/lsdc2.env\n")
	builder.WriteString(fmt.Sprintf("AWS_REGION=%s\n", stack.AwsRegion))
	for key, value := range env {
		builder.WriteString(fmt.Sprintf("%s=%s\n", key, value))
	}
	builder.WriteString("EOF\n")
	builder.WriteString("systemctl start lsdc2.service\n")

	userDataScript := builder.String()
	userDatab64 := base64.StdEncoding.EncodeToString([]byte(userDataScript))

	// Get cheapest subnet
	instanceTypeAndSubnetsFromCheapest, err := GetInstanceTypeAndSubnetSortedByPrice(stack, ec2Spec.InstanceTypes)
	if err != nil {
		return "", err
	}

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ec2.NewFromConfig(cfg)

	startInstanceTypeAndSubnet := func(instAndSn instanceTypeAndSubnet) (string, error) {
		result, err := client.RunInstances(context.TODO(), &ec2.RunInstancesInput{
			ImageId: aws.String(amiID),
			IamInstanceProfile: &ec2Types.IamInstanceProfileSpecification{
				Arn: aws.String(stack.Ec2VMProfileArn),
			},
			BlockDeviceMappings: []ec2Types.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/sda1"),
					Ebs: &ec2Types.EbsBlockDevice{
						VolumeType: ec2Types.VolumeTypeGp3,
						Iops:       aws.Int32(min(max(3000, ec2Spec.Iops), 6000)),
						Throughput: aws.Int32(min(max(125, ec2Spec.Throughput), 600)),
					},
				},
			},
			InstanceMarketOptions: &ec2Types.InstanceMarketOptionsRequest{
				MarketType: ec2Types.MarketTypeSpot,
			},
			InstanceType:      instAndSn.InstanceType,
			MaxCount:          aws.Int32(1),
			MinCount:          aws.Int32(1),
			SecurityGroupIds:  []string{spec.SecurityGroup},
			SubnetId:          aws.String(instAndSn.Subnet),
			TagSpecifications: ec2Tags(ec2Types.ResourceTypeInstance, map[string]string{"lsdc2.gamename": spec.Name}),
			UserData:          aws.String(userDatab64),
		})
		if err != nil {
			return "", err
		}
		if len(result.Instances) == 0 {
			return "", errors.New("instance creation returned nil error but empty results")
		}
		return *result.Instances[0].InstanceId, nil
	}
	for _, instAndSn := range instanceTypeAndSubnetsFromCheapest {
		instanceId, err := startInstanceTypeAndSubnet(instAndSn)

		// This is the happy path, where we have an instance ID and no error
		if err == nil {
			return instanceId, nil
		}

		// This is the unplanned path, where the function fail
		if !isInsufficientCapacityError(err) {
			return "", err
		}

		// And here we loop while the error type is InsufficientCapacityError
	}

	return "", errors.New("no instance started")
}

func isInsufficientCapacityError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InsufficientInstanceCapacity"
	}

	return false
}

// SendCommand sends a shell command to the specified EC2 instance using AWS SSM
func SendCommand(instanceID string, command string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ssm.NewFromConfig(cfg)

	_, err = client.SendCommand(context.TODO(), &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceID},
		Parameters: map[string][]string{
			"commands": {command},
		},
	})

	return err
}

// DescribeInstance retrieves the details of the specified EC2 instance.
func DescribeInstance(instanceID string) (ec2Types.Instance, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return ec2Types.Instance{}, err
	}
	client := ec2.NewFromConfig(cfg)

	result, err := client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return ec2Types.Instance{}, err
	}
	if len(result.Reservations) == 0 {
		return ec2Types.Instance{}, nil
	}
	if len(result.Reservations[0].Instances) == 0 {
		return ec2Types.Instance{}, nil
	}
	return result.Reservations[0].Instances[0], nil
}

// GetAmiID retrieves the AMI ID for the specified AMI name.
// Returns an error if no matching AMI is found.
func GetAmiID(amiName string) (string, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ec2.NewFromConfig(cfg)

	out, err := client.DescribeImages(context.TODO(), &ec2.DescribeImagesInput{
		Filters: []ec2Types.Filter{
			{
				Name:   aws.String("name"),
				Values: []string{amiName},
			},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.Images) == 0 {
		return "", fmt.Errorf("no ami found with name %s", amiName)
	}

	return *out.Images[0].ImageId, nil
}

// GetInstanceTypeAndSubnetSortedByPrice retrieves all possible instance type
// and subnet options, sorted by price, for the specified stack and instance types.
func GetInstanceTypeAndSubnetSortedByPrice(stack Lsdc2Stack, instanceTypes []ec2Types.InstanceType) ([]instanceTypeAndSubnet, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}
	client := ec2.NewFromConfig(cfg)

	// Get AZ from each subnet
	outSubnets, err := client.DescribeSubnets(context.TODO(), &ec2.DescribeSubnetsInput{
		SubnetIds: stack.Subnets,
	})
	if err != nil {
		return nil, err
	}

	instAndSn := make([]instanceTypeAndSubnet, len(instanceTypes)*len(stack.Subnets))
	idx := 0
	for _, sn := range outSubnets.Subnets {
		for _, instanceType := range instanceTypes {
			out, err := client.DescribeSpotPriceHistory(context.TODO(), &ec2.DescribeSpotPriceHistoryInput{
				AvailabilityZone:    sn.AvailabilityZone,
				InstanceTypes:       []ec2Types.InstanceType{instanceType},
				ProductDescriptions: []string{"Linux/UNIX"},
				EndTime:             aws.Time(time.Now()),
				MaxResults:          aws.Int32(1),
			})
			if err != nil {
				return nil, err
			}
			if len(out.SpotPriceHistory) > 0 {
				floatPrice, err := strconv.ParseFloat(*out.SpotPriceHistory[0].SpotPrice, 64)
				if err != nil {
					return nil, err
				}
				instAndSn[idx] = instanceTypeAndSubnet{
					InstanceType: out.SpotPriceHistory[0].InstanceType,
					Subnet:       *sn.SubnetId,
					Price:        floatPrice,
				}
				idx = idx + 1
			}
		}
	}

	instAndSn = instAndSn[:idx]
	sort.Slice(instAndSn, func(i, j int) bool {
		return instAndSn[i].Price < instAndSn[j].Price
	})

	return instAndSn, nil
}

// RestoreEbsBaseline resets the volume performance of the specified instance to
// the default gp3 values.
// TODO: replace hardcoded default gp3 values with dynamic configuration.
func RestoreEbsBaseline(instanceID string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ec2.NewFromConfig(cfg)

	vm, err := DescribeInstance(instanceID)
	if err != nil {
		return err
	}

	volumeID := vm.BlockDeviceMappings[0].Ebs.VolumeId

	_, err = client.ModifyVolume(context.TODO(), &ec2.ModifyVolumeInput{
		VolumeId:   volumeID,
		Iops:       aws.Int32(3000),
		Throughput: aws.Int32(125),
	})

	return err
}

// CreateSecurityGroup creates a new security group in AWS EC2. The security
// group is given the name and ingress from the specified ServerSpec. It is
// attached to the VPC of the LSDC2 stack.
// Returns the ID of the created security group.
func CreateSecurityGroup(spec ServerSpec, stack Lsdc2Stack) (groupID string, err error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ec2.NewFromConfig(cfg)

	result, err := client.CreateSecurityGroup(context.TODO(), &ec2.CreateSecurityGroupInput{
		TagSpecifications: ec2Tags(ec2Types.ResourceTypeSecurityGroup, map[string]string{"lsdc2.gamename": spec.Name}),
		GroupName:         aws.String(spec.Name),
		Description:       aws.String(fmt.Sprintf("Security group for LSDC2 %s", spec.Name)),
		VpcId:             aws.String(stack.Vpc),
	})
	if err != nil {
		groupID = ""
		return
	}

	// Create ingress rules
	_, err = client.AuthorizeSecurityGroupIngress(context.TODO(), &ec2.AuthorizeSecurityGroupIngressInput{
		TagSpecifications: ec2Tags(ec2Types.ResourceTypeSecurityGroupRule, map[string]string{"lsdc2.gamename": spec.Name}),
		GroupId:           result.GroupId,
		IpPermissions:     spec.AwsIpPermissionSpec(),
	})
	if err != nil {
		groupID = ""
		return
	}

	groupID = *result.GroupId
	err = nil
	return
}

// EnsureAndWaitSecurityGroupDeletion deletes the specified security group
// and waits until it is fully removed. The function retries up to 5 times
// with a 2-second interval between attempts.
func EnsureAndWaitSecurityGroupDeletion(groupName string, stack Lsdc2Stack) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ec2.NewFromConfig(cfg)

	descInput := &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2Types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{stack.Vpc}},
			{Name: aws.String("group-name"), Values: []string{groupName}},
		},
	}
	sg, err := client.DescribeSecurityGroups(context.TODO(), descInput)
	if err != nil {
		return err
	}
	if len(sg.SecurityGroups) != 0 {
		DeleteSecurityGroup(*sg.SecurityGroups[0].GroupId)
	}

	// Hacky sleep with hardcoded max tries and duration.
	// The loop break free if client.DescribeSecurityGroups
	// return and empty list.
	maxTries := 5
	tries := 0
	for {
		if tries > maxTries {
			return errors.New("wait timeout")
		}
		time.Sleep(time.Second * 2)
		sg, err = client.DescribeSecurityGroups(context.TODO(), descInput)
		if err != nil {
			return err
		}
		if len(sg.SecurityGroups) == 0 {
			return nil
		}
		tries++
	}
}

// DeleteSecurityGroup deletes the specified AWS EC2 security group
func DeleteSecurityGroup(groupID string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ec2.NewFromConfig(cfg)

	_, err = client.DeleteSecurityGroup(context.TODO(), &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(groupID),
	})
	return err
}

//===== Section: S3

// PresignGetS3Object generates a pre-signed URL for downloading an object
// from the specified S3 bucket and key. The URL expires after the given duration.
func PresignGetS3Object(bucket string, key string, expire time.Duration) (string, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := s3.NewFromConfig(cfg)

	presignClient := s3.NewPresignClient(client)
	req, err := presignClient.PresignGetObject(context.TODO(),
		&s3.GetObjectInput{
			Bucket:              aws.String(bucket),
			Key:                 aws.String(key),
			ResponseContentType: aws.String("application/octet-stream"),
		},
		s3.WithPresignExpires(expire),
	)
	if err != nil {
		return "", err
	}
	if req == nil {
		return "", errors.New("PresignGetObject returned a nil request")
	}
	return req.URL, nil
}

// PresignPutS3Object generates a pre-signed URL for uploading an object
// to the specified S3 bucket and key. The URL expires after the given duration.
func PresignPutS3Object(bucket string, key string, expire time.Duration) (string, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := s3.NewFromConfig(cfg)

	presignClient := s3.NewPresignClient(client)
	req, err := presignClient.PresignPutObject(context.TODO(),
		&s3.PutObjectInput{
			Bucket:      aws.String(bucket),
			Key:         aws.String(key),
			ContentType: aws.String("application/octet-stream"),
		},
		s3.WithPresignExpires(expire),
	)
	if err != nil {
		return "", err
	}
	if req == nil {
		return "", errors.New("PresignPutObject returned a nil request")
	}
	return req.URL, nil
}

// PresignMultipartUploadS3Object generates pre-signed URLs for uploading an object
// in multiple parts to the specified S3 bucket and key. The final URL is for completing
// the multipart upload. URLs expire after the given duration.
//
// FIXME: Replace with SDK v2 when it supports presigning multipart upload completion.
func PresignMultipartUploadS3Object(bucket string, key string, parts int, expire time.Duration) ([]string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	client := s3v1.New(sess)

	mpReply, err := client.CreateMultipartUpload(&s3v1.CreateMultipartUploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return nil, err
	}

	urls := make([]string, parts+1)
	for idx := range parts {
		req, _ := client.UploadPartRequest(&s3v1.UploadPartInput{
			Bucket:     aws.String(bucket),
			Key:        aws.String(key),
			UploadId:   mpReply.UploadId,
			PartNumber: aws.Int64(int64(idx + 1)),
		})
		url, err := req.Presign(expire)
		if err != nil {
			return nil, err
		}
		urls[idx] = url
	}
	req, _ := client.CompleteMultipartUploadRequest(&s3v1.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: mpReply.UploadId,
	})
	completeUrl, err := req.Presign(expire)
	if err != nil {
		return nil, err
	}
	urls[parts] = completeUrl

	return urls, nil
}
