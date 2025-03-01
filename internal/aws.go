package internal

import (
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/ssm"
)

//===== Section: SSM

// GetParameter retrieves the value of a parameter from AWS Systems Manager Parameter Store.
// The parameter is assumed to be encrypted using AWS managed key.
//
// Parameters:
//   - name: The name of the parameter to retrieve.
//
// Returns:
//   - string: The value of the parameter.
//   - error: An error if the parameter could not be retrieved.
func GetParameter(name string) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ssm.New(sess)

	input := &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	}
	param, err := svc.GetParameter(input)
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
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := sqs.New(sess)

	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueUrl),
		MessageBody: aws.String(msg),
	}
	_, err := svc.SendMessage(input)

	return err
}

//===== Section: DynamoDB

// DynamodbGetItem retrieves an item from a DynamoDB table.
//
// Parameters:
//   - tableName: The name of the DynamoDB table.
//   - key: The key of the item to retrieve.
//   - out: A pointer to the variable where the retrieved item will be unmarshaled.
//
// Returns:
//   - error: If any operation fails.
func DynamodbGetItem(tableName string, key string, out any) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := dynamodb.New(sess)

	input := &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]*dynamodb.AttributeValue{
			"key": {
				S: aws.String(key),
			},
		},
	}
	rawOut, err := svc.GetItem(input)
	if err != nil {
		return err
	}
	return dynamodbattribute.UnmarshalMap(rawOut.Item, out)
}

// DynamodbPutItem inserts an item into a specified DynamoDB table.
//
// Parameters:
//   - tableName: The name of the DynamoDB table where the item will be inserted.
//   - item: The item to be inserted into the table.
//
// Returns:
//   - error: If any operation fails.
func DynamodbPutItem(tableName string, item any) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := dynamodb.New(sess)

	av, err := dynamodbattribute.MarshalMap(item)
	if err != nil {
		return err
	}

	input := &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      av,
	}
	_, err = svc.PutItem(input)
	return err
}

// DynamodbDeleteItem deletes an item from a DynamoDB table.
//
// Parameters:
//   - tableName: The name of the DynamoDB table from which the item will be deleted.
//   - key: The key of the item to be deleted.
//
// Returns:
//   - error: If the deletion fails.
func DynamodbDeleteItem(tableName string, key string) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := dynamodb.New(sess)

	input := &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]*dynamodb.AttributeValue{
			"key": {
				S: aws.String(key),
			},
		},
	}
	_, err := svc.DeleteItem(input)
	return err
}

// DynamodbScanDo scans a DynamoDB table and processes each item using the
// provided function. The function takes a table name and a callback function
// as parameters. The callback functionis called for each item in the table
// and should return a boolean indicating whether to continue scanning and
// an error if any.
//
// Type Parameters:
//   - T: The type of the items in the DynamoDB table.
//
// Parameters:
//   - tableName: The name of the DynamoDB table to scan.
//   - fn: A callback function that processes each item. It takes a typed item as
//     input and returns a boolean indicating whether to continue scanning and an error if any.
//
// Returns:
//   - error: If the scan operation or the callback function encounters an error.
func DynamodbScanDo[T any](tableName string, fn func(typedItem T) (bool, error)) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := dynamodb.New(sess)

	var innerErr error
	outerErr := svc.ScanPages(&dynamodb.ScanInput{
		TableName: aws.String(tableName),
	}, func(page *dynamodb.ScanOutput, last bool) bool {
		var keepPaging bool
		var typedItem T
		for _, item := range page.Items {
			innerErr = dynamodbattribute.UnmarshalMap(item, &typedItem)
			if innerErr != nil {
				innerErr = fmt.Errorf("ScanPages / UnmarshalMap / %w", innerErr)
				return false // stop paging
			}
			keepPaging, innerErr = fn(typedItem)
			if innerErr != nil {
				innerErr = fmt.Errorf("ScanPages / fn / %w", innerErr)
				return false // stop paging
			}
			if !keepPaging {
				return false // stop paging
			}
		}

		return true // keep paging
	})
	if innerErr != nil {
		return innerErr
	}

	return outerErr
}

// DynamodbScanFindFirst scans a DynamoDB table and finds the first item that
// matches the specified key and value.
//
// Parameters:
//   - tableName: The name of the DynamoDB table to scan.
//   - key: The key to match in the DynamoDB items.
//   - value: The value to match for the specified key.
//   - out: A pointer to the variable where the retrieved item will be unmarshaled.
//
// Returns:
//   - error: If the scan or unmarshal operation fails.
func DynamodbScanFindFirst(tableName string, key string, value string, out any) (err error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := dynamodb.New(sess)

	var innerErr error
	outerErr := svc.ScanPages(&dynamodb.ScanInput{
		TableName: aws.String(tableName),
	}, func(page *dynamodb.ScanOutput, last bool) bool {
		for _, item := range page.Items {
			if val, ok := item[key]; ok {
				if val.S != nil && *val.S == value {
					innerErr = dynamodbattribute.UnmarshalMap(item, out)
					return false // stop paging
				}
			}
		}
		return true // keep paging
	})
	if innerErr != nil {
		return fmt.Errorf("ScanPages / UnmarshalMap / %w", innerErr)
	}

	return outerErr
}

// DynamodbScanAttr scans a DynamoDB table and retrieves a column of values
// for the specified argument column.
//
// Parameters:
//   - tableName: The name of the DynamoDB table to scan.
//   - column: The column to retrieve values from.
//
// Returns:
//   - []string: A slice of strings containing the values of the specified attribute key.
//   - error: If the scan operation fails.
func DynamodbScanAttr(tableName string, column string) ([]string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := dynamodb.New(sess)

	out := []string{}

	err := svc.ScanPages(&dynamodb.ScanInput{
		TableName: aws.String(tableName),
	}, func(page *dynamodb.ScanOutput, last bool) bool {
		outPage := make([]string, len(page.Items))
		for idx, item := range page.Items {
			if val, ok := item[column]; ok {
				if val.S != nil {
					outPage[idx] = *val.S
				}
			}
		}
		out = append(out, outPage...)

		return true // keep paging
	})

	return out, err
}

//===== Section: ECS

// Default ECS tag value
func ecsTags() []*ecs.Tag {
	return []*ecs.Tag{
		{
			Key:   aws.String("lsdc2-src"),
			Value: aws.String("discord"),
		},
	}
}

// RegisterTask registers a new ECS task definition with the specified parameters.
//
// Parameters:
//   - instName: The name of the task definition family.
//   - spec: The server specification containing CPU, memory, image,
//     environment variables, and port mappings.
//   - stack: The stack configuration containing task role ARN, execution
//     role ARN, and log group.
//
// Returns:
//   - error: If the task definition registration fails,.
func RegisterTask(instName string, spec ServerSpec, stack Lsdc2Stack) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ecs.New(sess)

	input := &ecs.RegisterTaskDefinitionInput{
		Tags:                    ecsTags(),
		Family:                  aws.String(instName),
		Cpu:                     aws.String(spec.Cpu),
		Memory:                  aws.String(spec.Memory),
		NetworkMode:             aws.String("awsvpc"),
		TaskRoleArn:             aws.String(stack.TaskRoleArn),
		ExecutionRoleArn:        aws.String(stack.ExecutionRoleArn),
		RequiresCompatibilities: []*string{aws.String("FARGATE")},
		RuntimePlatform: &ecs.RuntimePlatform{
			CpuArchitecture:       aws.String("X86_64"),
			OperatingSystemFamily: aws.String("LINUX"),
		},
		ContainerDefinitions: []*ecs.ContainerDefinition{
			{
				Essential:    aws.Bool(true),
				Image:        aws.String(spec.Image),
				Name:         aws.String(spec.Name + "_container"),
				Environment:  spec.AwsEnvSpec(),
				PortMappings: spec.AwsPortSpec(),
				LogConfiguration: &ecs.LogConfiguration{
					LogDriver: aws.String("awslogs"),
					Options: map[string]*string{
						"awslogs-group":         aws.String(stack.LogGroup),
						"awslogs-region":        aws.String("eu-west-3"), // FIXME: remove hardcoded region
						"awslogs-stream-prefix": aws.String("ecs"),
					},
				},
			},
		},
	}
	_, err := svc.RegisterTaskDefinition(input)

	return err
}

// DeregisterTaskFamily deregisters all task definitions within a specified ECS task family.
//
// Parameters:
//   - taskFamily: The name of the ECS task family to deregister.
//
// Returns:
//   - error: If any operation fails.
func DeregisterTaskFamily(taskFamily string) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ecs.New(sess)

	listInput := &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(taskFamily),
	}
	taskList, err := svc.ListTaskDefinitions(listInput)
	if err != nil {
		return err
	}

	for _, def := range taskList.TaskDefinitionArns {
		deregInput := &ecs.DeregisterTaskDefinitionInput{
			TaskDefinition: def,
		}
		_, err := svc.DeregisterTaskDefinition(deregInput)
		if err != nil {
			return err
		}
	}

	return err
}

// StartTask starts an ECS task using the provided server instance and stack configuration.
// It returns the ARN of the started task or an error if the task could not be started.
//
// Parameters:
//   - inst: ServerInstance containing the security group and task family information.
//   - stack: Lsdc2Stack containing the cluster and subnet information.
//
// Returns:
//   - string: The ARN of the started task.
//   - error: If the taks could not be started.
func StartTask(inst ServerInstance, stack Lsdc2Stack) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ecs.New(sess)

	subnets := make([]*string, len(stack.Subnets))
	for idx, sn := range stack.Subnets {
		subnets[idx] = aws.String(sn)
	}

	input := &ecs.RunTaskInput{
		Tags: ecsTags(),
		CapacityProviderStrategy: []*ecs.CapacityProviderStrategyItem{
			{CapacityProvider: aws.String("FARGATE_SPOT")},
		},
		Cluster: aws.String(stack.Cluster),
		Count:   aws.Int64(1),
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				AssignPublicIp: aws.String("ENABLED"),
				SecurityGroups: []*string{aws.String(inst.SecurityGroup)},
				Subnets:        subnets,
			},
		},
		TaskDefinition: aws.String(inst.TaskFamily),
	}
	result, err := svc.RunTask(input)
	if err != nil {
		return "", err
	}
	if len(result.Tasks) == 0 {
		return "", errors.New("task creation returned empty results")
	}

	return *result.Tasks[0].TaskArn, nil
}

// StopTask stops a running ECS task for a given server instance and stack configuration.
//
// Parameters:
//   - inst: The ServerInstance containing the TaskArn of the task to be stopped.
//   - stack: The Lsdc2Stack containing the ECS cluster and subnets information.
//
// Returns:
//   - error: If the task could not be stopped.
func StopTask(inst ServerInstance, stack Lsdc2Stack) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ecs.New(sess)

	subnets := make([]*string, len(stack.Subnets))
	for idx, sn := range stack.Subnets {
		subnets[idx] = aws.String(sn)
	}

	input := &ecs.StopTaskInput{
		Cluster: aws.String(stack.Cluster),
		Task:    aws.String(inst.TaskArn),
	}
	_, err := svc.StopTask(input)

	return err
}

// DescribeTask retrieves the details of a specific ECS task.
//
// Parameters:
//   - inst: The ServerInstance containing the TaskArn of the task to be described.
//   - stack: The Lsdc2Stack containing the ECS cluster information.
//
// Returns:
//   - *ecs.Task: A pointer to the ECS task details.
//   - error: If the task description fails.
func DescribeTask(inst ServerInstance, stack Lsdc2Stack) (*ecs.Task, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ecs.New(sess)

	inputDt := &ecs.DescribeTasksInput{
		Cluster: aws.String(stack.Cluster),
		Tasks:   []*string{aws.String(inst.TaskArn)},
	}
	resultDt, err := svc.DescribeTasks(inputDt)
	if err != nil {
		return nil, err
	}
	if len(resultDt.Tasks) == 0 {
		return nil, nil
	}
	return resultDt.Tasks[0], nil
}

//===== Section: EC2

// Default EC2 tag value
func ec2Tags(resType string) []*ec2.TagSpecification {
	return []*ec2.TagSpecification{
		{
			ResourceType: aws.String(resType),
			Tags: []*ec2.Tag{
				{
					Key:   aws.String("lsdc2-src"),
					Value: aws.String("discord"),
				},
			},
		},
	}
}

// CreateSecurityGroup creates a new security group in AWS EC2 with the specified
// server specifications and stack configuration. It sets up the security group
// with the provided name and description, associates it with the given VPC, and
// applies the specified ingress rules.
//
// Parameters:
//   - spec: ServerSpec containing the specifications for the server, including
//     the name and AWS IP permissions.
//   - stack: Lsdc2Stack containing the stack configuration, including the VPC ID.
//
// Returns:
//   - string: The ID of the created security group.
//   - error: If any operation failed.
func CreateSecurityGroup(spec ServerSpec, stack Lsdc2Stack) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ec2.New(sess)

	inputSg := &ec2.CreateSecurityGroupInput{
		TagSpecifications: ec2Tags("security-group"),
		GroupName:         aws.String(spec.Name),
		Description:       aws.String(fmt.Sprintf("Security group for LSDC2 %s", spec.Name)),
		VpcId:             aws.String(stack.Vpc),
	}
	resultSg, err := svc.CreateSecurityGroup(inputSg)
	if err != nil {
		return "", err
	}

	// Create ingress rules
	inputIngress := &ec2.AuthorizeSecurityGroupIngressInput{
		TagSpecifications: ec2Tags("security-group-rule"),
		GroupId:           resultSg.GroupId,
		IpPermissions:     spec.AwsIpPermissionSpec(),
	}
	_, err = svc.AuthorizeSecurityGroupIngress(inputIngress)
	if err != nil {
		return "", err
	}

	return *resultSg.GroupId, nil
}

// EnsureAndWaitSecurityGroupDeletion delete the specified security group,
// and implement a small wait loop until the DescribeSecurityGroups call
// return an empty list.
//
// The waiting is hardcoded: it runs 5 times with a 2 second wait between tries.
//
// Parameters:
//   - groupName: The name of the security group to delete.
//   - stack: The Lsdc2Stack containing the VPC information.
//
// Returns:
//   - error: If any operation fail, or if the wait times out.
func EnsureAndWaitSecurityGroupDeletion(groupName string, stack Lsdc2Stack) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ec2.New(sess)

	descInput := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(stack.Vpc)},
			},
			{
				Name:   aws.String("group-name"),
				Values: []*string{aws.String(groupName)},
			},
		},
	}
	sg, err := svc.DescribeSecurityGroups(descInput)
	if err != nil {
		return err
	}
	if len(sg.SecurityGroups) != 0 {
		DeleteSecurityGroup(*sg.SecurityGroups[0].GroupId)
	}

	// Hacky sleep with hardcoded max tries and duration.
	// The loop break free if svc.DescribeSecurityGroups
	// return and empty list.
	maxTries := 5
	tries := 0
	for {
		if tries > maxTries {
			return fmt.Errorf("wait timeout")
		}
		time.Sleep(time.Second * 2)
		sg, err = svc.DescribeSecurityGroups(descInput)
		if err != nil {
			return err
		}
		if len(sg.SecurityGroups) == 0 {
			return nil
		}
		tries++
	}
}

// DeleteSecurityGroup deletes an AWS EC2 security group.
//
// Parameters:
//   - groupID: The ID of the security group to be deleted.
//
// Returns:
//   - error: If the deletion fails.
func DeleteSecurityGroup(groupID string) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ec2.New(sess)

	input := &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(groupID),
	}
	_, err := svc.DeleteSecurityGroup(input)
	return err
}

// GetTaskIP retrieves the public IP address of an ECS task's ENI (Elastic
// Network Interface).
//
// Parameters:
//   - task: A pointer to an ecs.Task object representing the ECS task.
//
// Returns:
//   - string: The public IP address of the task's ENI.
//   - error: If any operation fail, or if the IP can't be determined.
func GetTaskIP(task *ecs.Task) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ec2.New(sess)

	// Get the ENI from the attachments
	if len(task.Attachments) == 0 {
		return "", errors.New("no ENI attached")
	}
	if *task.Attachments[0].Status != "ATTACHED" {
		return "", errors.New("ENI not in ATTACHED state")
	}
	var eniID *string
	for _, kv := range task.Attachments[0].Details {
		if *kv.Name == "networkInterfaceId" {
			eniID = kv.Value
		}
	}

	// Then describe IP from ENI
	inputDni := &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{
			eniID,
		},
	}
	resultDni, err := svc.DescribeNetworkInterfaces(inputDni)
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

//===== Section: S3

// PresignGetS3Object generates a pre-signed URL for downloading an object
// from an S3 bucket.
//
// Parameters:
//   - bucket: The name of the S3 bucket.
//   - key: The key of the object in the S3 bucket.
//   - expire: The duration for which the pre-signed URL will be valid.
//
// Returns:
//   - string: The pre-signed URL.
//   - error: If the URL could not be generated.
func PresignGetS3Object(bucket string, key string, expire time.Duration) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := s3.New(sess)

	input := &s3.GetObjectInput{
		Bucket:              aws.String(bucket),
		Key:                 aws.String(key),
		ResponseContentType: aws.String("application/octet-stream"),
	}
	req, _ := svc.GetObjectRequest(input)
	url, err := req.Presign(expire)
	if err != nil {
		return "", nil
	}
	return url, nil
}

// PresignPutS3Object generates a pre-signed URL for uploading an object to an S3 bucket.
//
// Parameters:
//   - bucket: The name of the S3 bucket.
//   - key: The key within the S3 bucket where the object will be stored.
//   - expire: The duration for which the pre-signed URL will be valid.
//
// Returns:
//   - string: The pre-signed URL.
//   - error: If the URL could not be generated.
func PresignPutS3Object(bucket string, key string, expire time.Duration) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := s3.New(sess)

	input := &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String("application/octet-stream"),
	}
	req, _ := svc.PutObjectRequest(input)
	url, err := req.Presign(expire)
	if err != nil {
		return "", nil
	}
	return url, nil
}
