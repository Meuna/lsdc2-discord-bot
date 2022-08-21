package internal

import (
	"errors"
	"fmt"
	"time"

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

//
// SSM helpers
//

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

//
// SQS helpers
//

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

//
// DynamoDB helpers
//

func DynamodbGetItem(tableName string, key string, out interface{}) error {
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

func DynamodbPutItem(tableName string, item interface{}) error {
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

func DynamodbScanFind(tableName string, key string, value string, out interface{}) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := dynamodb.New(sess)

	err := svc.ScanPages(&dynamodb.ScanInput{
		TableName: aws.String(tableName),
	}, func(page *dynamodb.ScanOutput, last bool) bool {
		for _, item := range page.Items {
			if val, ok := item[key]; ok {
				if *val.S == value {
					dynamodbattribute.UnmarshalMap(item, out)
					return false
				}
			}
		}

		return true // keep paging
	})

	return err
}

func DynamodbScanAttr(tableName string, key string) ([]string, error) {
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
			if val, ok := item[key]; ok {
				outPage[idx] = *val.S
			}
		}
		out = append(out, outPage...)

		return true // keep paging
	})

	return out, err
}

//
// ECS helpers
//

func ecsTags() []*ecs.Tag {
	return []*ecs.Tag{
		{
			Key:   aws.String("lsdc2-src"),
			Value: aws.String("discord"),
		},
	}
}

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
						"awslogs-region":        aws.String("eu-west-3"),
						"awslogs-stream-prefix": aws.String("ecs"),
					},
				},
			},
		},
	}
	_, err := svc.RegisterTaskDefinition(input)

	return err
}

func DeregisterTaskFamiliy(taskFamily string) error {
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

	return *result.Tasks[0].TaskArn, err
}

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

//
// EC2 helpers
//

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

func GetTaskIP(task *ecs.Task, stack Lsdc2Stack) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ec2.New(sess)

	// Get the ENI from the attachments
	if len(task.Attachments) == 0 {
		return "", errors.New("no ENI attached")
	}
	if *task.Attachments[0].Status != "ATTACHED" {
		return "", errors.New("ENI not in attached")
	}
	var eniID *string
	for _, kv := range task.Attachments[0].Details {
		if *kv.Name == "networkInterfaceId" {
			eniID = kv.Value
		}
	}

	// And finally describe IP from ENI
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

//
// S3 helpers
//

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
