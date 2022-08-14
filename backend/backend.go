package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"lsdc2/discordbot/internal"
	"net/http"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/bwmarrin/discordgo"
)

func main() {
	ctx := context.Background()
	ctx = context.WithValue(ctx, "bot", InitBackend())

	lambda.StartWithContext(ctx, handleEvent)
}

type Event struct {
	events.SQSEvent
	events.CloudWatchEvent
}

func handleEvent(ctx context.Context, event Event) error {
	bot := ctx.Value("bot").(Backend)

	if event.Source == "" {
		fmt.Println("Received SQS event")
		bot.handleSQSEvent(event.SQSEvent)
	} else {
		fmt.Println("Received '%s' CloudWatch event", event.DetailType)
		bot.handleCloudWatchEvent(event.CloudWatchEvent)
	}

	// We make the bot unable to fail: events will never get back to the queue
	return nil
}

func InitBackend() Backend {
	bot, err := internal.ParseEnv()
	if err != nil {
		panic(err)
	}
	return Backend{bot}
}

type Backend struct {
	internal.BotEnv
}

func (bot Backend) handleSQSEvent(event events.SQSEvent) {
	for _, msg := range event.Records {
		cmd, err := internal.UnmarshallQueuedAction(msg)
		if err != nil {
			fmt.Printf("Error %s with msg: %+v\n", err, msg)
		} else {
			bot.routeFcn(cmd)
		}
	}
}

func (bot Backend) handleCloudWatchEvent(event events.CloudWatchEvent) {
	switch event.DetailType {
	case "ECS Task State Change":
		bot.notifyTaskUpdate(event)
	default:
		fmt.Printf("%s event not handled\n", event.DetailType)
	}
}

//
//	Bot reply
//

func (bot Backend) message(channelID string, msg string, fmtarg ...interface{}) {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		return
	}

	_, err = sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: fmt.Sprintf(msg, fmtarg...),
	})
	if err != nil {
		fmt.Println("InteractionResponseEdit failed", err)
		return
	}
}

func (bot Backend) followUp(cmd internal.BackendCmd, msg string, fmtarg ...interface{}) {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		return
	}

	itn := discordgo.Interaction{
		AppID: cmd.AppID,
		Token: cmd.Token,
	}
	_, err = sess.InteractionResponseEdit(&itn, &discordgo.WebhookEdit{
		Content: fmt.Sprintf(msg, fmtarg...),
	})
	if err != nil {
		fmt.Println("InteractionResponseEdit failed", err)
		return
	}
}

//
//	Backend commands
//

func (bot Backend) routeFcn(cmd internal.BackendCmd) {
	switch cmd.Action() {
	case internal.RegisterGameAPI:
		bot.registerGame(cmd)

	case internal.BootstrapAPI:
		bot.bootstrapGuild(cmd)

	case internal.SpinupAPI:
		bot.spinupServer(cmd)

	case internal.DestroyAPI:
		bot.destroyServer(cmd)

	default:
		fmt.Printf("Unrecognized function %s\n", cmd.Action())
	}
}

func (bot Backend) registerGame(cmd internal.BackendCmd) {
	args := cmd.Args.(*internal.RegisterGameArgs)
	fmt.Printf("Received game register request with args %+v\n", args)

	// Dispatch spec from URL or cmd
	var specJson []byte
	if len(args.SpecUrl) > 0 {
		fmt.Printf("Registerting: spec download %s\n", args.SpecUrl)
		resp, err := http.Get(args.SpecUrl)
		if err != nil {
			fmt.Println("http.Get failed", err)
			bot.followUp(cmd, "🚫 Failed to get URL %s", args.SpecUrl)
			return
		}
		defer resp.Body.Close()

		specJson, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Println("ioutil.ReadAll failed", err)
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
	} else if len(args.Spec) > 0 {
		specJson = []byte(args.Spec)
	} else {
		fmt.Println("Both spec inputs are empty")
		bot.followUp(cmd, "🚫 no spec provided")
		return
	}

	// Parse spec
	fmt.Printf("Registerting: parse spec\n")
	spec := internal.ServerSpec{}
	err := json.Unmarshal(specJson, &spec)
	if err != nil {
		fmt.Println("Unmarshal failed", err)
		bot.followUp(cmd, "🚫 Spec parsing error")
		return
	}

	missingFields := spec.MissingField()
	if len(missingFields) > 0 {
		fmt.Printf("Spec if missing field %s\n", missingFields)
		bot.followUp(cmd, "🚫 Spec if missing field %s", missingFields)
		return
	}

	// Check existing spec and abort/cleanup if necessary
	fmt.Printf("Registerting %s: scan game list\n", spec.Name)
	gameList, err := internal.DynamodbScanAttr(bot.SpecTable, "key")

	if internal.Contains(gameList, spec.Name) && !args.Overwrite {
		fmt.Printf("Registerting %s: aborted, spec already exists\n", spec.Name)
		bot.followUp(cmd, "🚫 Game %s already registered and overwrite=False", spec.Name)
		return
	}

	// Security group creation
	fmt.Printf("Registerting %s: ensure previous security group deletion\n", spec.Name)
	if err = internal.EnsureAndWaitSecurityGroupDeletion(spec.Name, bot.Lsdc2Stack); err != nil {
		fmt.Println("EnsureAndWaitSecurityGroupDeletion failed", err)
	}
	fmt.Printf("Registerting %s: create security group\n", spec.Name)
	sgID, err := internal.CreateSecurityGroup(spec, bot.Lsdc2Stack)
	if err != nil {
		fmt.Println("CreateSecurityGroup failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	spec.SecurityGroup = sgID

	// Retrieve spinup command
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	fmt.Printf("Registerting %s: lookup spinup command\n", spec.Name)
	allCmd, err := sess.ApplicationCommands(cmd.AppID, "")
	if err != nil {
		fmt.Println("ApplicationCommands failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	var spinupCmd *discordgo.ApplicationCommand
	for _, cmd := range allCmd {
		if cmd.Name == internal.SpinupAPI {
			spinupCmd = cmd
			break
		}
	}
	if spinupCmd == nil {
		fmt.Println("spinup cmd not found")
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// spinup command options update
	gameList = append(gameList, spec.Name)
	spinupCmd.Options[0].Choices = make([]*discordgo.ApplicationCommandOptionChoice, len(gameList))
	for idx, gameName := range gameList {
		spinupCmd.Options[0].Choices[idx] = &discordgo.ApplicationCommandOptionChoice{
			Value: gameName,
			Name:  gameName,
		}
	}
	_, err = sess.ApplicationCommandEdit(cmd.AppID, "", spinupCmd.ID, spinupCmd)
	if err != nil {
		fmt.Println("ApplicationCommandEdit failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Finally, persist the spec in db
	fmt.Printf("Registerting %s: dp register\n", spec.Name)
	err = internal.DynamodbPutItem(bot.SpecTable, spec)
	if err != nil {
		fmt.Println("DynamodbPutItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s register done !", spec.Name)
}

func (bot Backend) bootstrapGuild(cmd internal.BackendCmd) {
	args := cmd.Args.(*internal.BootstrapArgs)
	fmt.Printf("Received bootstraping request with args %+v\n", args)

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	fmt.Printf("Bootstraping %s: command creation\n", args.GuildID)
	if internal.CreateGuildCommands(sess, cmd.AppID, args.GuildID); err != nil {
		fmt.Println("CreateGuildCommands failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	fmt.Printf("Bootstraping %s: LSDC2 category\n", args.GuildID)
	lsdc2Category, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name: "LSDC2",
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		fmt.Println("GuildChannelCreateComplex failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	fmt.Printf("Bootstraping %s: admin channel\n", args.GuildID)
	_, err = sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     "Administration",
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: lsdc2Category.ID,
	})
	if err != nil {
		fmt.Println("GuildChannelCreateComplex failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	fmt.Printf("Bootstraping %s: welcome channel\n", args.GuildID)
	_, err = sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     "Welcome",
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: lsdc2Category.ID,
	})
	if err != nil {
		fmt.Println("GuildChannelCreateComplex failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ Boostrap done !")
}

func (bot Backend) spinupServer(cmd internal.BackendCmd) {
	args := cmd.Args.(*internal.SpinupArgs)
	fmt.Printf("Received server creation request with args %+v\n", args)

	fmt.Printf("Create %s/%s: get spec\n", args.GuildID, args.GameName)
	spec := internal.ServerSpec{}
	if err := internal.DynamodbGetItem(bot.SpecTable, args.GameName, &spec); err != nil {
		fmt.Println("DynamodbGetItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if spec.Name == "" {
		fmt.Printf("Create %s/%s: missing spec\n", args.GuildID, args.GameName)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	instName := fmt.Sprintf("%s-%d", args.GameName, spec.ServerCount)
	taskFamily := fmt.Sprintf("lsdc2-%s-%s", args.GuildID, instName)

	fmt.Printf("Create %s/%s: increment spec count\n", args.GuildID, args.GameName)
	spec.ServerCount = spec.ServerCount + 1
	if err := internal.DynamodbPutItem(bot.SpecTable, spec); err != nil {
		fmt.Println("DynamodbPutItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		return
	}

	fmt.Printf("Create %s/%s: chan creation\n", args.GuildID, args.GameName)
	channel, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name: instName,
		Type: discordgo.ChannelTypeGuildText,
	})
	if err != nil {
		fmt.Println("GuildChannelCreateComplex failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	fmt.Printf("Create %s/%s: task register\n", args.GuildID, args.GameName)
	if spec.EnvMap == nil {
		spec.EnvMap = map[string]string{}
	}
	spec.EnvMap["LSDC2_BUCKET"] = bot.SaveGameBucket
	spec.EnvMap["LSDC2_KEY"] = instName
	for key, value := range args.Env {
		spec.EnvMap[key] = value
	}
	if err = internal.RegisterTask(taskFamily, spec, bot.Lsdc2Stack); err != nil {
		fmt.Println("RegisterTask failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	fmt.Printf("Create %s/%s: register instance\n", args.GuildID, args.GameName)
	inst := internal.ServerInstance{
		Name:          instName,
		SpecName:      spec.Name,
		ChannelID:     channel.ID,
		TaskFamily:    taskFamily,
		SecurityGroup: spec.SecurityGroup,
	}
	if err = internal.DynamodbPutItem(bot.InstanceTable, inst); err != nil {
		fmt.Println("DynamodbPutItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
	}

	bot.followUp(cmd, "✅ %s server creation done !", args.GameName)
}

func (bot Backend) destroyServer(cmd internal.BackendCmd) {
	args := cmd.Args.(*internal.DestroyArgs)
	fmt.Printf("Received server creation request with args %+v\n", args)

	fmt.Printf("Destroy %s: get inst\n", args.ChannelID)
	inst := internal.ServerInstance{}
	if err := internal.DynamodbGetItem(bot.InstanceTable, args.ChannelID, &inst); err != nil {
		fmt.Println("DynamodbGetItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	fmt.Printf("Destroy %s: channel delete\n", args.ChannelID)
	if _, err = sess.ChannelDelete(args.ChannelID); err != nil {
		fmt.Println("ChannelDelete failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	fmt.Printf("Destroy %s: task unregister\n", args.ChannelID)
	if err = internal.DeregisterTaskFamiliy(inst.TaskFamily); err != nil {
		fmt.Println("DeregisterTaskFamiliy failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	fmt.Printf("Destroy %s: unregister instance\n", args.ChannelID)
	if err = internal.DynamodbDeleteItem(bot.InstanceTable, inst.ChannelID); err != nil {
		fmt.Println("DynamodbDeleteItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ Server destruction done !")
}

//
//	CloudWatch events
//

func (bot Backend) notifyTaskUpdate(event events.CloudWatchEvent) {
	fmt.Println("Received ECS task state")

	task := ecs.Task{}
	json.Unmarshal(event.Detail, &task)

	inst := internal.ServerInstance{}
	err := internal.DynamodbScanFind(bot.InstanceTable, "taskArn", *task.TaskArn, &inst)
	if err != nil {
		fmt.Println("DynamodbGetItem failed", err)
		return
	}

	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		fmt.Println("DynamodbGetItem failed", err)
		return
	}

	switch internal.GetTaskStatus(&task) {
	case internal.TaskProvisioning:
		bot.message(inst.ChannelID, "📢 Server task state: %s", *task.LastStatus)
	case internal.TaskRunning:
		ip, err := internal.GetTaskIP(&task, bot.Lsdc2Stack)
		if err != nil {
			ip = "error retrieving ip"
		}
		bot.message(inst.ChannelID, "📢 Server task state: %s\nIP: %s (open ports: %s)",
			*task.LastStatus, ip, spec.OpenPorts())
	case internal.TaskContainerStopping:
		bot.message(inst.ChannelID, "📢 Server task is going offline")
	case internal.TaskStopped:
		bot.message(inst.ChannelID, "📢 Server task went offline")
	}
}
