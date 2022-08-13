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
	"github.com/bwmarrin/discordgo"
)

func main() {
	ctx := context.Background()
	ctx = context.WithValue(ctx, "bot", InitBackend())

	lambda.StartWithContext(ctx, handleEvent)
}

func handleEvent(ctx context.Context, event events.SQSEvent) error {
	bot := ctx.Value("bot").(Backend)

	for _, msg := range event.Records {
		cmd, err := internal.UnmarshallQueuedAction(msg)
		if err != nil {
			fmt.Printf("Error %s with msg: %+v\n", err, msg)
		} else {
			bot.routeFcn(cmd)
		}
	}
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

func (bot Backend) routeFcn(cmd internal.BackendCmd) {
	switch cmd.Action() {
	case internal.RegisterGameAPI:
		bot.registerGame(cmd)

	case internal.BootstrapAPI:
		bot.bootstrapGuild(cmd)

	case internal.SpinupAPI:
		bot.createServer(cmd)

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
			return
		}
		defer resp.Body.Close()

		specJson, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Println("ioutil.ReadAll failed", err)
			return
		}
	} else if len(args.Spec) > 0 {
		specJson = []byte(args.Spec)
	} else {
		fmt.Println("Both spec inputs are empty")
		return
	}

	// Parse spec
	fmt.Printf("Registerting: parse spec\n")
	spec := internal.ServerSpec{}
	err := json.Unmarshal(specJson, &spec)
	if err != nil {
		fmt.Println("Unmarshal failed", err)
		return
	}

	if err = spec.EnsureComplete(); err != nil {
		fmt.Println("EnsureComplete failed", err)
		return
	}

	// Check existing spec and abort/cleanup if necessary
	fmt.Printf("Registerting %s: scan game list\n", spec.Name)
	gameList, err := internal.DynamodbScanAttr(bot.SpecTable, "name")

	if internal.Contains(gameList, spec.Name) && !args.Overwrite {
		fmt.Printf("Registerting %s: aborted, spec already exists\n", spec.Name)
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
		return
	}
	spec.SecurityGroup = sgID

	// Retrieve spinup command
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		return
	}
	fmt.Printf("Registerting %s: lookup spinup command\n", spec.Name)
	allCmd, err := sess.ApplicationCommands(cmd.AppID, "")
	if err != nil {
		fmt.Println("ApplicationCommands failed", err)
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
		return
	}

	// spinup command options update
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
		return
	}

	// Finally, persist the spec in db
	fmt.Printf("Registerting %s: dp register\n", spec.Name)
	err = internal.DynamodbPutItem(bot.SpecTable, spec)
	if err != nil {
		fmt.Println("DynamodbPutItem failed", err)
	}
}

func (bot Backend) bootstrapGuild(cmd internal.BackendCmd) {
	args := cmd.Args.(*internal.BootstrapArgs)
	fmt.Printf("Received bootstraping request with args %+v\n", args)

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		return
	}

	fmt.Printf("Bootstraping %s: command creation\n", args.GuildID)
	if internal.CreateGuildCommands(sess, cmd.AppID, args.GuildID); err != nil {
		fmt.Println("CreateGuildCommands failed", err)
		return
	}

	fmt.Printf("Bootstraping %s: LSDC2 category\n", args.GuildID)
	lsdc2Category, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name: "LSDC2",
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		fmt.Println("GuildChannelCreateComplex failed", err)
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
		return
	}
}

func (bot Backend) createServer(cmd internal.BackendCmd) {
	args := cmd.Args.(*internal.SpinupArgs)
	fmt.Printf("Received server creation request with args %+v\n", args)

	fmt.Printf("Create %s/%s: get spec\n", args.GuildID, args.GameName)
	spec := internal.ServerSpec{}
	if err := internal.DynamodbGetItem(bot.SpecTable, args.GameName, &spec); err != nil {
		fmt.Println("DynamodbGetItem failed", err)
		return
	}
	if spec.Name == "" {
		fmt.Printf("Create %s/%s: missing spec\n", args.GuildID, args.GameName)
		return
	}

	instName := fmt.Sprintf("%s-%d", args.GameName, spec.ServerCount)
	taskFamily := fmt.Sprintf("lsdc2-%s-%s", args.GuildID, instName)

	fmt.Printf("Create %s/%s: increment spec count\n", args.GuildID, args.GameName)
	spec.ServerCount = spec.ServerCount + 1
	if err := internal.DynamodbPutItem(bot.SpecTable, spec); err != nil {
		fmt.Println("DynamodbPutItem failed", err)
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
		return
	}

	fmt.Printf("Create %s/%s: register instance\n", args.GuildID, args.GameName)
	inst := internal.ServerInstance{
		Name:          instName,
		ChannelID:     channel.ID,
		TaskFamily:    taskFamily,
		SecurityGroup: spec.SecurityGroup,
	}
	if err = internal.DynamodbPutItem(bot.InstanceTable, inst); err != nil {
		fmt.Println("DynamodbPutItem failed", err)
	}
}

func (bot Backend) destroyServer(cmd internal.BackendCmd) {
	args := cmd.Args.(*internal.DestroyArgs)
	fmt.Printf("Received server creation request with args %+v\n", args)

	fmt.Printf("Destroy %s: get inst\n", args.ChannelID)
	inst := internal.ServerInstance{}
	if err := internal.DynamodbGetItem(bot.InstanceTable, args.ChannelID, &inst); err != nil {
		fmt.Println("DynamodbGetItem failed", err)
		return
	}

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		return
	}

	fmt.Printf("Destroy %s: channel delete\n", args.ChannelID)
	if _, err = sess.ChannelDelete(args.ChannelID); err != nil {
		fmt.Println("ChannelDelete failed", err)
	}

	fmt.Printf("Destroy %s: task unregister\n", args.ChannelID)
	if err = internal.DeregisterTaskFamiliy(inst.TaskFamily); err != nil {
		fmt.Println("DeregisterTaskFamiliy failed", err)
	}

	fmt.Printf("Destroy %s: unregister instance\n", args.ChannelID)
	if err = internal.DynamodbDeleteItem(bot.InstanceTable, inst.ChannelID); err != nil {
		fmt.Println("DynamodbDeleteItem failed", err)
	}
}
