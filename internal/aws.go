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
	"github.com/aws/aws-sdk-go/service/sqs"
)

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

func RegisterTask(instName string, spec ServerSpec, stack Lsdc2Stack) error {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ecs.New(sess)

	input := &ecs.RegisterTaskDefinitionInput{
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

func CreateSecurityGroup(spec ServerSpec, stack Lsdc2Stack) (string, error) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	svc := ec2.New(sess)

	inputSg := &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(spec.Name),
		Description: aws.String(fmt.Sprintf("Security group for LSDC2 %s", spec.Name)),
		VpcId:       aws.String(stack.Vpc),
	}
	resultSg, err := svc.CreateSecurityGroup(inputSg)
	if err != nil {
		return "", err
	}

	// Create ingress rules
	inputIngress := &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       resultSg.GroupId,
		IpPermissions: spec.AwsIpPermissionSpec(),
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
