package internal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
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
)

//===== Section: SSM

// GetParameter retrieves the value of a parameter from AWS Systems Manager Parameter Store.
// The parameter is assumed to be encrypted using AWS managed key.
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

// DynamodbPutItem inserts an item into a specified DynamoDB table
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

// DynamodbScanDo scans the specified DynamoDB table and return a list of unmarshalled items
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

// DynamodbScanFindFirst scans the specified DynamoDB table and finds the
// first item that matches the specified key and value. The item is
// unmarshalled into the provided out pointer.
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
func ecsTags() []ecsTypes.Tag {
	return []ecsTypes.Tag{
		{Key: aws.String("lsdc2-src"), Value: aws.String("discord")},
	}
}

// RegisterTaskFamily registers a new ECS task definition with the specified parameters.
//
// Parameters:
//   - region: The AWS region.
//   - srvName: The name of the task definition family.
//   - spec: The server specification containing CPU, memory, image,
//     environment variables, and port mappings.
//   - stack: The stack configuration containing task role ARN, execution
//     role ARN, and log group.
func RegisterTaskFamily(stack Lsdc2Stack, spec ServerSpec, env map[string]string, taskFamily string) error {
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
		Tags:                    ecsTags(),
		Family:                  aws.String(taskFamily),
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
				Essential:    aws.Bool(true),
				Image:        aws.String(ecsSpec.Image),
				Name:         aws.String(spec.Name + "_container"),
				Environment:  envArray,
				PortMappings: spec.AwsPortSpec(),
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
func DeregisterTaskFamily(taskFamily string) error {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return err
	}
	client := ecs.NewFromConfig(cfg)

	taskList, err := client.ListTaskDefinitions(context.TODO(), &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(taskFamily),
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

// StartEcsTask starts an ECS task for the provided family and security groupe.
// Returns the ARN of the started task.
func StartEcsTask(stack Lsdc2Stack, spec ServerSpec, taskFamily string) (arn string, err error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ecs.NewFromConfig(cfg)

	subnets := make([]string, len(stack.Subnets))
	copy(subnets, stack.Subnets)

	result, err := client.RunTask(context.TODO(), &ecs.RunTaskInput{
		Tags: ecsTags(),
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
		TaskDefinition: aws.String(taskFamily),
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

// StopEcsTask stops the provided ECS task
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

// DescribeTask retrieves the details of the provided ECS task
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

//===== Section: EC2

// Default EC2 tag value
func ec2Tags(resourceType ec2Types.ResourceType) []ec2Types.TagSpecification {
	return []ec2Types.TagSpecification{
		{
			ResourceType: resourceType,
			Tags: []ec2Types.Tag{
				{Key: aws.String("lsdc2-src"), Value: aws.String("discord")},
			},
		},
	}
}

// StartEc2VM starts a new EC2 instance for the provided server spec.
// Returns the ID of the started instance.
func StartEc2VM(stack Lsdc2Stack, spec ServerSpec, env map[string]string) (instanceID string, err error) {
	ec2Spec, ok := spec.Engine.(*Ec2Engine)
	if !ok {
		return "", errors.New("engine spec is not an EC2 engine")
	}

	// Build user data script
	var builder strings.Builder
	builder.WriteString("#!/bin/bash\n")
	builder.WriteString("cat << EOF >> /lsdc2/lsdc2.env\n")
	builder.WriteString(fmt.Sprintf("AWS_REGION=%s\n", stack.AwsRegion))
	for key, value := range env {
		builder.WriteString(fmt.Sprintf("%s=%s\n", key, value))
	}
	if os.Getenv("DEBUG") != "" {
		builder.WriteString("DEBUG=1\n")
	}
	builder.WriteString("EOF\n")
	builder.WriteString("systemctl start lsdc2.service\n")

	userDataScript := builder.String()
	userDatab64 := base64.StdEncoding.EncodeToString([]byte(userDataScript))

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ec2.NewFromConfig(cfg)

	input := &ec2.RunInstancesInput{
		ImageId: aws.String(ec2Spec.Ami),
		IamInstanceProfile: &ec2Types.IamInstanceProfileSpecification{
			Arn: aws.String(stack.Ec2VMProfileArn),
		},
		InstanceMarketOptions: &ec2Types.InstanceMarketOptionsRequest{
			MarketType: ec2Types.MarketTypeSpot,
		},
		InstanceType:      ec2Types.InstanceType(ec2Spec.InstanceType),
		MaxCount:          aws.Int32(1),
		MinCount:          aws.Int32(1),
		SecurityGroupIds:  []string{spec.SecurityGroup},
		SubnetId:          aws.String(stack.Subnets[0]), // FIXME: do something better
		TagSpecifications: ec2Tags(ec2Types.ResourceTypeInstance),
		UserData:          aws.String(userDatab64),
	}
	if ec2Spec.Storage > 0 {
		input.BlockDeviceMappings = []ec2Types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2Types.EbsBlockDevice{
					VolumeSize: aws.Int32(min(max(8, ec2Spec.Storage), 200)),
				},
			},
		}
	}
	result, err := client.RunInstances(context.TODO(), input)
	if err != nil {
		return "", err
	}
	if len(result.Instances) == 0 {
		instanceID = ""
		return "", errors.New("instance creation returned empty results")
	}

	instanceID = *result.Instances[0].InstanceId
	err = nil
	return
}

// SendCommand stops the specified EC2 instance
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

// DescribeInstance retrieves the details of the provided EC2 instance
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

// CreateSecurityGroup creates a new security group in AWS EC2. The security
// group is given the name and ingress from the specified ServerSpec. It is
// attached to the VPD of the LSDC2 stack.
//
// Returns the ID of the created security group.
func CreateSecurityGroup(spec ServerSpec, stack Lsdc2Stack) (groupID string, err error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", err
	}
	client := ec2.NewFromConfig(cfg)

	result, err := client.CreateSecurityGroup(context.TODO(), &ec2.CreateSecurityGroupInput{
		TagSpecifications: ec2Tags(ec2Types.ResourceTypeSecurityGroup),
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
		TagSpecifications: ec2Tags("security-group-rule"),
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

// EnsureAndWaitSecurityGroupDeletion deletes the specified security group,
// and implement a small retry/wait loop until the DescribeSecurityGroups call
// return an empty list.
//
// The waiting is hardcoded: it runs 5 times with a 2 second wait between tries.
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
			return fmt.Errorf("wait timeout")
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

// PresignGetS3Object generates a pre-signed URL for downloading the object
// from from the specified key and S3 bucket. The link expires after the
// specified duration.
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
		return "", fmt.Errorf("PresignGetObject returned a nil request")
	}
	return req.URL, nil
}

// PresignPutS3Object generates a pre-signed URL for uploading an object for
// the specified key and S3 bucket. The link expires after the specified duration.
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
		return "", fmt.Errorf("PresignPutObject returned a nil request")
	}
	return req.URL, nil
}

// PresignMultipartUploadS3Object generates a list of pre-signed URL for uploading
// an object in multiple parts for the specified key and S3 bucket. The last link is
// the CompletePart request. The links expires after the specified duration.
//
// FIXME: use SDK v2 when it is able to presign completed multipart upload
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
