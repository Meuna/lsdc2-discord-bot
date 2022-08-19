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
		fmt.Printf("Received '%s' CloudWatch event\n", event.DetailType)
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

	case internal.InviteAPI:
		bot.inviteMember(cmd)

	case internal.KickAPI:
		bot.kickMember(cmd)

	default:
		fmt.Printf("Unrecognized function %s\n", cmd.Action())
	}
}

//	Game registering

func (bot Backend) registerGame(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.RegisterGameArgs)
	fmt.Printf("Received game register request with args %+v\n", args)

	spec, err := bot._getSpec(cmd, args)
	if err != nil {
		fmt.Println("_getJsonSpec failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Check spec is not missing any mandatory field
	missingFields := spec.MissingField()
	if len(missingFields) > 0 {
		fmt.Printf("Spec if missing field %s\n", missingFields)
		bot.followUp(cmd, "🚫 Spec if missing field %s", missingFields)
		return
	}

	// Check existing spec and abort/cleanup if necessary
	fmt.Printf("Registerting %s: scan game list\n", spec.Name)
	gameList, err := internal.DynamodbScanAttr(bot.SpecTable, "key")
	if err != nil {
		fmt.Println("DynamodbScanAttr failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if !internal.Contains(gameList, spec.Name) {
		gameList = append(gameList, spec.Name)
	} else if !args.Overwrite {
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

	// Update spinup command
	if err := bot._updateSpinupOptions(cmd, args, spec.Name, gameList); err != nil {
		fmt.Println("_updateSpinupOptions failed", err)
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

func (bot Backend) _getSpec(cmd internal.BackendCmd, args internal.RegisterGameArgs) (spec internal.ServerSpec, err error) {
	var jsonSpec []byte
	// Dispatch spec source
	if len(args.SpecUrl) > 0 {
		// Spec is in args.SpecUrl
		fmt.Printf("Registerting: spec download %s\n", args.SpecUrl)
		resp, errZob := http.Get(args.SpecUrl)
		if err != nil {
			err = fmt.Errorf("http.Get failed: %s", errZob)
			return
		}
		defer resp.Body.Close()

		jsonSpec, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			err = fmt.Errorf("http.Get failed: %s", err)
			return
		}
	} else if len(args.Spec) > 0 {
		// Spec is in args.Spec
		jsonSpec = []byte(args.Spec)
	} else {
		err = fmt.Errorf("Both spec inputs are empty")
		return
	}

	// Parse spec
	fmt.Printf("Registerting: parse spec\n")
	if err = json.Unmarshal(jsonSpec, &spec); err != nil {
		err = fmt.Errorf("Unmarshal failed: %s", err)
		return
	}

	return
}

func (bot Backend) _updateSpinupOptions(cmd internal.BackendCmd, args internal.RegisterGameArgs, specName string, gameList []string) error {
	// Retrieve spinup command
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New failed", err)
	}
	fmt.Printf("Registerting %s: lookup spinup command\n", specName)
	globalCmd, err := sess.ApplicationCommands(cmd.AppID, "")
	if err != nil {
		return fmt.Errorf("ApplicationCommands failed: %s", err)
	}
	var spinupCmd *discordgo.ApplicationCommand
	for _, cmd := range globalCmd {
		if cmd.Name == internal.SpinupAPI {
			spinupCmd = cmd
			break
		}
	}
	if spinupCmd == nil {
		return fmt.Errorf("spinup cmd not found")
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
		return fmt.Errorf("ApplicationCommandEdit failed", err)
	}

	return nil
}

//	Game spinup

func (bot Backend) spinupServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.SpinupArgs)
	fmt.Printf("Received server spinup request with args %+v\n", args)

	// Get spec
	spec, err := bot._getSpecAndIncreaseCount(cmd, args)
	if err != nil {
		fmt.Println("_getSpecAndIncreaseCount failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	instName := fmt.Sprintf("%s-%d", args.GameName, spec.ServerCount)
	taskFamily := fmt.Sprintf("lsdc2-%s-%s", args.GuildID, instName)

	// Create server channel
	chanID, err := bot._createServerChannel(cmd, args, instName)
	if err != nil {
		fmt.Println("_createServerChannel failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Register ECS task
	fmt.Printf("Spinup %s/%s: task register\n", args.GuildID, args.GameName)
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

	// And register instance
	fmt.Printf("Spinup %s/%s: register instance\n", args.GuildID, args.GameName)
	inst := internal.ServerInstance{
		Name:          instName,
		SpecName:      spec.Name,
		ChannelID:     chanID,
		TaskFamily:    taskFamily,
		SecurityGroup: spec.SecurityGroup,
	}
	if err = internal.DynamodbPutItem(bot.InstanceTable, inst); err != nil {
		fmt.Println("DynamodbPutItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s server creation done !", args.GameName)
}

func (bot Backend) _getSpecAndIncreaseCount(cmd internal.BackendCmd, args internal.SpinupArgs) (spec internal.ServerSpec, err error) {
	fmt.Printf("Spinup %s/%s: get spec\n", args.GuildID, args.GameName)
	if err = internal.DynamodbGetItem(bot.SpecTable, args.GameName, &spec); err != nil {
		err = fmt.Errorf("DynamodbGetItem failed: %s", err)
		return
	}
	if spec.Name == "" {
		err = fmt.Errorf("missing spec")
		return
	}

	fmt.Printf("Spinup %s/%s: increment spec count\n", args.GuildID, args.GameName)
	spec.ServerCount = spec.ServerCount + 1
	if err = internal.DynamodbPutItem(bot.SpecTable, spec); err != nil {
		err = fmt.Errorf("DynamodbPutItem failed: %s", err)
		return
	}

	return
}

func (bot Backend) _createServerChannel(cmd internal.BackendCmd, args internal.SpinupArgs, instName string) (chanID string, err error) {
	// Retrieve guild conf
	fmt.Printf("Spinup %s/%s: get guild conf\n", args.GuildID, args.GameName)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		err = fmt.Errorf("DynamodbGetItem failed: %s", err)
		return
	}

	// Create chan
	sessBot, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		err = fmt.Errorf("discordgo.New failed: %s", err)
		return
	}
	fmt.Printf("Spinup %s/%s: chan creation\n", args.GuildID, args.GameName)
	channel, err := sessBot.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     instName,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: gc.ChannelCategoryID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			internal.PrivateChannelOverwrite(args.GuildID),
			internal.ViewAppcmdOverwrite(gc.AdminRoleID),
			internal.AppcmdOverwrite(gc.UserRoleID),
		},
	})
	if err != nil {
		err = fmt.Errorf("GuildChannelCreateComplex failed: %s", err)
		return
	}

	// Setup permissions
	scope := "applications.commands.permissions.update applications.commands.update"
	sessBearer, cleanup, err := internal.BearerSession(bot.ClientID, bot.ClientSecret, scope)
	if err != nil {
		err = fmt.Errorf("BearerSession failed: %s", err)
		return
	}
	defer cleanup()

	fmt.Printf("Spinup %s: setting command rights on channel\n", args.GuildID)
	guildCmd, err := sessBearer.ApplicationCommands(cmd.AppID, args.GuildID)
	if err != nil {
		err = fmt.Errorf("ApplicationCommands failed: %s", err)
		return
	}
	extendUserCmdFilter := append(internal.UserCmd, internal.InviteKickCmd...)
	extendUserCmd := internal.FilterCommandsByName(guildCmd, extendUserCmdFilter)

	err = internal.EnableChannelCommands(sessBearer, cmd.AppID, args.GuildID, channel.ID, extendUserCmd)
	if err != nil {
		err = fmt.Errorf("EnableChannelCommands failed: %s", err)
		return
	}

	chanID = channel.ID
	return
}

//	Game destroy

func (bot Backend) destroyServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.DestroyArgs)
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

//	Bootstraping

func (bot Backend) bootstrapGuild(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.BootstrapArgs)
	fmt.Printf("Received bootstraping request with args %+v\n", args)

	// Make sure the guild is not bootstrapped
	fmt.Printf("Bootstraping %s: check if guild already exists\n", args.GuildID)
	gcCheck := internal.GuildConf{}
	if err := internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gcCheck); err != nil {
		fmt.Println("DynamodbGetItem failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if gcCheck.GuildID != "" {
		fmt.Println("Guild already have an entry in bootstrap table")
		bot.followUp(cmd, "🚫 Guild already have an entry in bootstrap table")
		return
	}

	// Command registering
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("discordgo.New failed", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	fmt.Printf("Bootstraping %s: command creation\n", args.GuildID)
	if err := internal.SetupLsdc2Commands(sess, cmd.AppID, args.GuildID); err != nil {
		fmt.Printf("SetupLsdc2Commands failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	gc := internal.GuildConf{
		GuildID: args.GuildID,
	}
	if err := bot._createRoles(cmd, args, &gc); err != nil {
		fmt.Printf("_createRoles failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._createChannels(cmd, args, &gc); err != nil {
		fmt.Printf("_createChannels failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._setupPermissions(cmd, args, gc); err != nil {
		fmt.Printf("_setupPermissions failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Register conf
	fmt.Printf("Create %s: register instance\n", args.GuildID)
	if err := internal.DynamodbPutItem(bot.GuildTable, gc); err != nil {
		fmt.Printf("DynamodbPutItem failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
	}

	bot.followUp(cmd, "✅ Bootstrap complete !")
}

func (bot Backend) _createRoles(cmd internal.BackendCmd, args internal.BootstrapArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New failed: %s", err)
	}

	fmt.Printf("Bootstraping %s: LSDC2 roles\n", args.GuildID)
	adminRole, err := sess.GuildRoleCreate(args.GuildID)
	if err != nil {
		return fmt.Errorf("GuildRoleCreate failed: %s", err)
	}
	_, err = sess.GuildRoleEdit(args.GuildID, adminRole.ID, "LSDC2 Admin", 0x8833ff, true, 0, true)
	if err != nil {
		return fmt.Errorf("GuildRoleEdit failed: %s", err)
	}
	userRole, err := sess.GuildRoleCreate(args.GuildID)
	if err != nil {
		return fmt.Errorf("GuildRoleCreate failed: %s", err)
	}
	_, err = sess.GuildRoleEdit(args.GuildID, userRole.ID, "LSDC2 User", 0x33aaff, true, 0, true)
	if err != nil {
		return fmt.Errorf("GuildRoleEdit failed: %s", err)
	}

	gc.AdminRoleID = adminRole.ID
	gc.UserRoleID = userRole.ID

	return nil
}

func (bot Backend) _createChannels(cmd internal.BackendCmd, args internal.BootstrapArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New failed: %s", err)
	}

	fmt.Printf("Bootstraping %s: LSDC2 category\n", args.GuildID)
	lsdc2Category, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name: "LSDC2",
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex failed: %s", err)
	}
	fmt.Printf("Bootstraping %s: admin channel\n", args.GuildID)
	adminChan, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     "administration",
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: lsdc2Category.ID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			internal.PrivateChannelOverwrite(args.GuildID),
			internal.ViewAppcmdOverwrite(gc.AdminRoleID),
		},
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex failed: %s", err)
	}
	fmt.Printf("Bootstraping %s: welcome channel\n", args.GuildID)
	welcomeChan, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     "welcome",
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: lsdc2Category.ID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			internal.ViewInviteOverwrite(gc.AdminRoleID),
		},
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex failed: %s", err)
	}

	gc.ChannelCategoryID = lsdc2Category.ID
	gc.AdminChannelID = adminChan.ID
	gc.WelcomeChannelID = welcomeChan.ID

	return nil
}

func (bot Backend) _setupPermissions(cmd internal.BackendCmd, args internal.BootstrapArgs, gc internal.GuildConf) error {
	scope := "applications.commands.permissions.update applications.commands.update"
	sess, cleanup, err := internal.BearerSession(bot.ClientID, bot.ClientSecret, scope)
	if err != nil {
		return fmt.Errorf("BearerSession failed: %s", err)
	}
	defer cleanup()

	fmt.Printf("Bootstraping %s: setting commands rights\n", args.GuildID)
	allCmd, err := internal.GetAllCommands(sess, cmd.AppID, args.GuildID)
	if err != nil {
		return fmt.Errorf("GetAllCommands failed: %s", err)
	}
	ownerCmd := internal.FilterCommandsByName(allCmd, internal.OwnerCmd)
	adminCmd := internal.FilterCommandsByName(allCmd, internal.AdminCmd)
	userCmd := internal.FilterCommandsByName(allCmd, internal.UserCmd)

	err = internal.DisableCommands(sess, cmd.AppID, args.GuildID, ownerCmd)
	if err != nil {
		return fmt.Errorf("DisableCommands failed: %s", err)
	}
	err = internal.SetupAdminCommands(sess, cmd.AppID, args.GuildID, gc, adminCmd)
	if err != nil {
		return fmt.Errorf("SetupAdminCommands failed: %s", err)
	}
	err = internal.SetupUserCommands(sess, cmd.AppID, args.GuildID, gc, userCmd)
	if err != nil {
		return fmt.Errorf("SetupUserCommands failed: %s", err)
	}

	return nil
}

//
//  Invite/Kick command
//

func (bot Backend) inviteMember(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.InviteArgs)

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Printf("discordgo.New failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve requester membership
	fmt.Println("Invite: retrieve requester member")
	requester, err := sess.GuildMember(args.GuildID, args.RequesterID)
	if err != nil {
		fmt.Printf("GuildMember failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve target membership
	fmt.Println("Invite: retrieve target member")
	target, err := sess.GuildMember(args.GuildID, args.TargetID)
	if err != nil {
		fmt.Printf("GuildMember failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve LSDC2 Admin role from guild conf
	fmt.Printf("Invite %s by %s: get guild conf\n", target.User.Username, requester.User.Username)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve list of game channel
	fmt.Printf("Invite %s by %s: get list of channel\n", target.User.Username, requester.User.Username)
	serverChannelIDs, err := internal.DynamodbScanAttr(bot.InstanceTable, "key")
	if err != nil {
		fmt.Printf("DynamodbScanAttr failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if internal.Contains(requester.Roles, gc.AdminRoleID) || args.RequesterIsAdmin {
		fmt.Printf("Invite %s by %s: add member role\n", target.User.Username, requester.User.Username)
		sess.GuildMemberRoleAdd(args.GuildID, args.TargetID, gc.UserRoleID)
		bot.message(gc.WelcomeChannelID, ":call_me: Welcome %s !", target.User.Username)
	} else if !internal.Contains(target.Roles, gc.UserRoleID) {
		bot.followUp(cmd, "🚫 %s is not an allowed LSDC2 user", target.User.Username)
		return
	}
	if internal.Contains(serverChannelIDs, args.ChannelID) {
		fmt.Printf("Invite %s by %s: add member to channel\n", target.User.Username, requester.User.Username)
		internal.AddUserView(sess, args.ChannelID, args.TargetID)
	}

	bot.followUp(cmd, "%s added !", target.User.Username)
}

func (bot Backend) kickMember(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.KickArgs)

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Printf("discordgo.New failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve requester membership
	fmt.Printf("Kick: retrieve requester member")
	requester, err := sess.GuildMember(args.GuildID, args.RequesterID)
	if err != nil {
		fmt.Printf("GuildMember failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve target membership
	fmt.Printf("Kick: retrieve target member")
	target, err := sess.GuildMember(args.GuildID, args.TargetID)
	if err != nil {
		fmt.Printf("GuildMember failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve LSDC2 Admin role from guild conf
	fmt.Printf("Kick %s by %s: get guild conf", target.User.Username, requester.User.Username)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve list of game channel
	fmt.Printf("Invite %s by %s: get list of channel", target.User.Username, requester.User.Username)
	serverChannelIDs, err := internal.DynamodbScanAttr(bot.InstanceTable, "key")
	if err != nil {
		fmt.Printf("DynamodbScanAttr failed: %s\n", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if internal.Contains(requester.Roles, gc.AdminRoleID) || args.RequesterIsAdmin {
		if args.ChannelID == gc.AdminChannelID {
			fmt.Printf("Kick %s by %s: remove member role", target.User.Username, requester.User.Username)
			sess.GuildMemberRoleRemove(args.GuildID, args.TargetID, gc.UserRoleID)
			for _, channelID := range serverChannelIDs {
				internal.RemoveUserView(sess, channelID, args.TargetID)
			}
			bot.message(gc.WelcomeChannelID, ":middle_finger: in for face %s !", target.User.Username)
		} else {
			internal.RemoveUserView(sess, args.ChannelID, args.TargetID)
		}
	} else {
		bot.followUp(cmd, "🚫 not allowed")
		return
	}

	bot.followUp(cmd, "%s kicked !", target.User.Username)
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
