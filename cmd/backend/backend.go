package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/meuna/lsdc2-discord-bot/internal"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/bwmarrin/discordgo"
)

func main() {
	ctx := context.Background()
	ctx = context.WithValue(ctx, "bot", InitBackend())

	lambda.StartWithOptions(handleEvent, lambda.WithContext(ctx))
}

type Event struct {
	events.SQSEvent
	events.CloudWatchEvent
}

func handleEvent(ctx context.Context, event Event) error {
	bot := ctx.Value("bot").(Backend)

	if len(event.SQSEvent.Records) > 0 {
		bot.handleSQSEvent(event.SQSEvent)
	} else if event.CloudWatchEvent.DetailType != "" {
		bot.handleCloudWatchEvent(event.CloudWatchEvent)
	} else {
		fmt.Println("Could not discriminate event type")
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
	fmt.Println("Received SQS event")

	for _, msg := range event.Records {
		cmd, err := internal.UnmarshallQueuedAction(msg)
		if err != nil {
			fmt.Printf("error internal.UnmarshallQueuedAction for msg %+v / %s\n", msg, err)
		} else {
			bot.routeFcn(cmd)
		}
	}
}

func (bot Backend) handleCloudWatchEvent(event events.CloudWatchEvent) {
	fmt.Printf("Received '%s' CloudWatch event\n", event.DetailType)

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
		fmt.Println("error discordgo.New /", err)
		return
	}

	_, err = sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: fmt.Sprintf(msg, fmtarg...),
	})
	if err != nil {
		fmt.Println("error ChannelMessageSendComplex /", err)
		return
	}
}

func (bot Backend) followUp(cmd internal.BackendCmd, msg string, fmtarg ...interface{}) {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("error discordgo.New /", err)
		return
	}

	itn := discordgo.Interaction{
		AppID: cmd.AppID,
		Token: cmd.Token,
	}
	_, err = sess.InteractionResponseEdit(&itn, &discordgo.WebhookEdit{
		Content: internal.Pointer(fmt.Sprintf(msg, fmtarg...)),
	})
	if err != nil {
		fmt.Println("error InteractionResponseEdit /", err)
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

	case internal.GoodbyeAPI:
		bot.goodbyeGuild(cmd)

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
		fmt.Println("error _getSpec /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Check spec is not missing any mandatory field
	missingFields := spec.MissingField()
	if len(missingFields) > 0 {
		fmt.Printf("error spec is missing field %s\n", missingFields)
		bot.followUp(cmd, "🚫 Spec if missing field %s", missingFields)
		return
	}

	// Check existing spec and abort/cleanup if necessary
	fmt.Printf("Registerting %s: scan game list\n", spec.Name)
	gameList, err := internal.DynamodbScanAttr(bot.SpecTable, "key")
	if err != nil {
		fmt.Println("error internal.DynamodbScanAttr /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if internal.Contains(gameList, spec.Name) && !args.Overwrite {
		fmt.Printf("Registerting %s: aborted, spec already exists\n", spec.Name)
		bot.followUp(cmd, "🚫 Game %s already registered and overwrite=False", spec.Name)
		return
	}

	// Security group creation
	fmt.Printf("Registerting %s: ensure previous security group deletion\n", spec.Name)
	if err = internal.EnsureAndWaitSecurityGroupDeletion(spec.Name, bot.Lsdc2Stack); err != nil {
		fmt.Println("error EnsureAndWaitSecurityGroupDeletion /", err)
	}
	fmt.Printf("Registerting %s: create security group\n", spec.Name)
	sgID, err := internal.CreateSecurityGroup(spec, bot.Lsdc2Stack)
	if err != nil {
		fmt.Println("error CreateSecurityGroup /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	spec.SecurityGroup = sgID

	// Finally, persist the spec in db
	fmt.Printf("Registerting %s: dp register\n", spec.Name)
	err = internal.DynamodbPutItem(bot.SpecTable, spec)
	if err != nil {
		fmt.Println("error DynamodbPutItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s register done !", spec.Name)
}

func (bot Backend) _getSpec(cmd internal.BackendCmd, args internal.RegisterGameArgs) (spec internal.ServerSpec, err error) {
	var jsonSpec []byte

	// Dispatch spec source
	if len(args.SpecUrl) > 0 {
		var resp *http.Response

		// Spec is in args.SpecUrl
		fmt.Printf("Registerting: spec download %s\n", args.SpecUrl)
		resp, err = http.Get(args.SpecUrl)
		if err != nil {
			err = fmt.Errorf("http.Get / %s", err)
			return
		}
		defer resp.Body.Close()

		jsonSpec, err = io.ReadAll(resp.Body)
		if err != nil {
			err = fmt.Errorf("io.ReadAll / %s", err)
			return
		}
	} else if len(args.Spec) > 0 {
		// Spec is in args.Spec
		jsonSpec = []byte(args.Spec)
	} else {
		err = fmt.Errorf("both spec inputs are empty")
		return
	}

	// Parse spec
	fmt.Printf("Registerting: parse spec\n")
	if err = json.Unmarshal(jsonSpec, &spec); err != nil {
		err = fmt.Errorf("json.Unmarshal / %s", err)
		return
	}

	return
}

//	Game spinup

func (bot Backend) spinupServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.SpinupArgs)
	fmt.Printf("Received server spinup request with args %+v\n", args)

	// Get spec
	spec, err := bot._getSpecAndIncreaseCount(cmd, args)
	if err != nil {
		fmt.Println("error _getSpecAndIncreaseCount /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	instName := fmt.Sprintf("%s-%d", args.GameName, spec.ServerCount)
	taskFamily := fmt.Sprintf("lsdc2-%s-%s", args.GuildID, instName)

	// Create server channel
	chanID, err := bot._createServerChannel(cmd, args, instName)
	if err != nil {
		fmt.Println("error _createServerChannel /", err)
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
		fmt.Println("error RegisterTask /", err)
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
		fmt.Println("error DynamodbPutItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s server creation done !", args.GameName)
}

func (bot Backend) _getSpecAndIncreaseCount(cmd internal.BackendCmd, args internal.SpinupArgs) (spec internal.ServerSpec, err error) {
	fmt.Printf("Spinup %s/%s: get spec\n", args.GuildID, args.GameName)
	if err = internal.DynamodbGetItem(bot.SpecTable, args.GameName, &spec); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %s", err)
		return
	}
	if spec.Name == "" {
		err = fmt.Errorf("missing spec")
		return
	}

	fmt.Printf("Spinup %s/%s: increment spec count\n", args.GuildID, args.GameName)
	spec.ServerCount = spec.ServerCount + 1
	if err = internal.DynamodbPutItem(bot.SpecTable, spec); err != nil {
		err = fmt.Errorf("DynamodbPutItem / %s", err)
		return
	}

	return
}

func (bot Backend) _createServerChannel(cmd internal.BackendCmd, args internal.SpinupArgs, instName string) (chanID string, err error) {
	// Retrieve guild conf
	fmt.Printf("Spinup %s/%s: get guild conf\n", args.GuildID, args.GameName)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %s", err)
		return
	}

	// Create chan
	sessBot, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		err = fmt.Errorf("discordgo.New / %s", err)
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
		err = fmt.Errorf("GuildChannelCreateComplex / %s", err)
		return
	}

	// Setup permissions
	scope := "applications.commands.permissions.update applications.commands.update"
	sessBearer, cleanup, err := internal.BearerSession(bot.ClientID, bot.ClientSecret, scope)
	if err != nil {
		err = fmt.Errorf("BearerSession / %s", err)
		return
	}
	defer cleanup()

	fmt.Printf("Spinup %s: setting command rights on channel\n", args.GuildID)
	guildCmd, err := sessBearer.ApplicationCommands(cmd.AppID, args.GuildID)
	if err != nil {
		err = fmt.Errorf("ApplicationCommands / %s", err)
		return
	}
	extendUserCmdFilter := append(internal.UserCmd, internal.InviteKickCmd...)
	extendUserCmd := internal.CommandsWithNameInList(guildCmd, extendUserCmdFilter)

	err = internal.EnableChannelCommands(sessBearer, cmd.AppID, args.GuildID, channel.ID, extendUserCmd)
	if err != nil {
		err = fmt.Errorf("EnableChannelCommands / %s", err)
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
		fmt.Println("error DynamodbGetItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	err := bot._destroyServerInstance(inst)
	if err != nil {
		fmt.Println("error _destroyServerInstance /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ Server destruction done !")
}

func (bot Backend) _destroyServerInstance(inst internal.ServerInstance) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("error discordgo.New / %s", err)
	}

	fmt.Printf("Destroy %s: channel delete\n", inst.ChannelID)
	if _, err = sess.ChannelDelete(inst.ChannelID); err != nil {
		return fmt.Errorf("error ChannelDelete / %s", err)
	}

	fmt.Printf("Destroy %s: task unregister\n", inst.ChannelID)
	if err = internal.DeregisterTaskFamiliy(inst.TaskFamily); err != nil {
		return fmt.Errorf("error DeregisterTaskFamiliy / %s", err)
	}

	fmt.Printf("Destroy %s: unregister instance\n", inst.ChannelID)
	if err = internal.DynamodbDeleteItem(bot.InstanceTable, inst.ChannelID); err != nil {
		return fmt.Errorf("error DynamodbDeleteItem / %s", err)
	}

	return nil
}

//	Bootstraping

func (bot Backend) bootstrapGuild(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.BootstrapArgs)
	fmt.Printf("Received bootstraping request with args %+v\n", args)

	// Make sure the guild is not bootstrapped
	fmt.Printf("Bootstraping %s: check if guild already exists\n", args.GuildID)
	gcCheck := internal.GuildConf{}
	if err := internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gcCheck); err != nil {
		fmt.Println("error DynamodbGetItem /", err)
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
		fmt.Println("error discordgo.New /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	fmt.Printf("Bootstraping %s: command creation\n", args.GuildID)
	if err := internal.CreateGuildsCommands(sess, cmd.AppID, args.GuildID); err != nil {
		fmt.Println("error CreateGuildsCommands /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	gc := internal.GuildConf{
		GuildID: args.GuildID,
	}
	if err := bot._createRoles(cmd, args, &gc); err != nil {
		fmt.Println("error _createRoles /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._createChannels(cmd, args, &gc); err != nil {
		fmt.Println("error _createChannels /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._setupPermissions(cmd, args, gc); err != nil {
		fmt.Println("error _setupPermissions /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Register conf
	fmt.Printf("Bootstraping %s: register instance\n", args.GuildID)
	if err := internal.DynamodbPutItem(bot.GuildTable, gc); err != nil {
		fmt.Println("error DynamodbPutItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
	}

	bot.followUp(cmd, "✅ Bootstrap complete !")
}

func (bot Backend) _createRoles(cmd internal.BackendCmd, args internal.BootstrapArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %s", err)
	}

	fmt.Printf("Bootstraping %s: LSDC2 roles\n", args.GuildID)
	adminRole, err := sess.GuildRoleCreate(args.GuildID, &discordgo.RoleParams{
		Name:        "LSDC2 Admin",
		Color:       internal.Pointer(0x8833ff),
		Hoist:       internal.Pointer(true),
		Mentionable: internal.Pointer(true),
	})
	if err != nil {
		return fmt.Errorf("GuildRoleCreate / %s", err)
	}
	userRole, err := sess.GuildRoleCreate(args.GuildID, &discordgo.RoleParams{
		Name:        "LSDC2 User",
		Color:       internal.Pointer(0x33aaff),
		Hoist:       internal.Pointer(true),
		Mentionable: internal.Pointer(true),
	})
	if err != nil {
		return fmt.Errorf("GuildRoleCreate / %s", err)
	}

	gc.AdminRoleID = adminRole.ID
	gc.UserRoleID = userRole.ID

	return nil
}

func (bot Backend) _createChannels(cmd internal.BackendCmd, args internal.BootstrapArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %s", err)
	}

	fmt.Printf("Bootstraping %s: LSDC2 category\n", args.GuildID)
	lsdc2Category, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name: "LSDC2",
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex / %s", err)
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
		return fmt.Errorf("GuildChannelCreateComplex / %s", err)
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
		return fmt.Errorf("GuildChannelCreateComplex / %s", err)
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
		return fmt.Errorf("BearerSession / %s", err)
	}
	defer cleanup()

	fmt.Printf("Bootstraping %s: setting commands rights\n", args.GuildID)
	registeredCmd, err := sess.ApplicationCommands(cmd.AppID, args.GuildID)
	if err != nil {
		return fmt.Errorf("discordgo.ApplicationCommands / %s", err)
	}

	adminCmd := internal.CommandsWithNameInList(registeredCmd, internal.AdminCmd)
	userCmd := internal.CommandsWithNameInList(registeredCmd, internal.UserCmd)

	err = internal.SetupAdminCommands(sess, cmd.AppID, args.GuildID, gc, adminCmd)
	if err != nil {
		return fmt.Errorf("SetupAdminCommands / %s", err)
	}
	err = internal.SetupUserCommands(sess, cmd.AppID, args.GuildID, gc, userCmd)
	if err != nil {
		return fmt.Errorf("SetupUserCommands / %s", err)
	}

	return nil
}

//
//  Goodbyeing
//

func (bot Backend) goodbyeGuild(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.GoodbyeArgs)
	fmt.Printf("Received goodbye request with args %+v\n", args)

	// Make sure the guild is bootstrapped
	fmt.Printf("Goodbyeing %s: check if guild exists\n", args.GuildID)
	gc := internal.GuildConf{}
	if err := internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		fmt.Println("error DynamodbGetItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if gc.GuildID == "" {
		fmt.Println("Guild does not any entry in bootstrap table")
		bot.followUp(cmd, "🚫 Guild not does have any entry in bootstrap table")
		return
	}

	// Destroying all games
	fmt.Printf("Goodbyeing %s: destroying games\n", args.GuildID)
	var err error
	err = internal.DynamodbScanDo(bot.InstanceTable, func(item map[string]*dynamodb.AttributeValue) bool {
		inst := internal.ServerInstance{}
		if err = dynamodbattribute.UnmarshalMap(item, &inst); err != nil {
			fmt.Println("error DynamodbScanDo / UnmarshalMap /", err)
			return false // stop paging
		}

		err = bot._destroyServerInstance(inst)
		if err != nil {
			fmt.Println("error DynamodbScanDo / _destroyServerInstance /", err)
			return false // stop paging
		}

		return true // keep paging
	})
	if err != nil {
		fmt.Println("error DynamodbScanDo /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Command de-registering
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		fmt.Println("error discordgo.New /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	fmt.Printf("Goodbyeing %s: command deletion\n", args.GuildID)
	if err := internal.DeleteGuildsCommands(sess, cmd.AppID, args.GuildID); err != nil {
		fmt.Println("error DeleteGuildsCommands /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if err := bot._deleteChannels(args, &gc); err != nil {
		fmt.Println("error _deleteChannels /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._deleteRoles(args, &gc); err != nil {
		fmt.Println("error _deleteRoles /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// De-register conf
	fmt.Printf("Goodbyeing %s: register instance\n", args.GuildID)
	if err := internal.DynamodbDeleteItem(bot.GuildTable, gc.GuildID); err != nil {
		fmt.Println("error DynamodbDeleteItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
	}

	bot.followUp(cmd, "✅ Goodbye complete !")
}

func (bot Backend) _deleteChannels(args internal.GoodbyeArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %s", err)
	}

	fmt.Printf("Goodbyeing %s: LSDC2 category %s\n", args.GuildID, gc.ChannelCategoryID)
	_, err = sess.ChannelDelete(gc.ChannelCategoryID)
	if err != nil {
		return fmt.Errorf("ChannelDelete / %s", err)
	}
	fmt.Printf("Goodbyeing %s: admin channel %s\n", args.GuildID, gc.AdminChannelID)
	_, err = sess.ChannelDelete(gc.AdminChannelID)
	if err != nil {
		return fmt.Errorf("ChannelDelete / %s", err)
	}
	fmt.Printf("Goodbyeing %s: welcome channel %s\n", args.GuildID, gc.WelcomeChannelID)
	_, err = sess.ChannelDelete(gc.WelcomeChannelID)
	if err != nil {
		return fmt.Errorf("ChannelDelete / %s", err)
	}

	return nil
}

func (bot Backend) _deleteRoles(args internal.GoodbyeArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %s", err)
	}

	fmt.Printf("Goodbyeing %s: LSDC2 roles\n", args.GuildID)
	err = sess.GuildRoleDelete(args.GuildID, gc.AdminRoleID)
	if err != nil {
		return fmt.Errorf("GuildRoleDelete / %s", err)
	}
	err = sess.GuildRoleDelete(args.GuildID, gc.UserRoleID)
	if err != nil {
		return fmt.Errorf("GuildRoleDelete / %s", err)
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
		fmt.Println("error discordgo.New /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve requester membership
	fmt.Println("Invite: retrieve requester member")
	requester, err := sess.GuildMember(args.GuildID, args.RequesterID)
	if err != nil {
		fmt.Println("error GuildMember /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve target membership
	fmt.Println("Invite: retrieve target member")
	target, err := sess.GuildMember(args.GuildID, args.TargetID)
	if err != nil {
		fmt.Println("error GuildMember /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve LSDC2 Admin role from guild conf
	fmt.Printf("Invite %s by %s: get guild conf\n", target.User.Username, requester.User.Username)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		fmt.Println("error DynamodbGetItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve list of game channel
	fmt.Printf("Invite %s by %s: get list of channel\n", target.User.Username, requester.User.Username)
	serverChannelIDs, err := internal.DynamodbScanAttr(bot.InstanceTable, "key")
	if err != nil {
		fmt.Println("error DynamodbScanAttr /", err)
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
		fmt.Println("error discordgo.New /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve requester membership
	fmt.Printf("Kick: retrieve requester member")
	requester, err := sess.GuildMember(args.GuildID, args.RequesterID)
	if err != nil {
		fmt.Println("error GuildMember /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve target membership
	fmt.Printf("Kick: retrieve target member")
	target, err := sess.GuildMember(args.GuildID, args.TargetID)
	if err != nil {
		fmt.Println("error GuildMember /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve LSDC2 Admin role from guild conf
	fmt.Printf("Kick %s by %s: get guild conf", target.User.Username, requester.User.Username)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		fmt.Println("error DynamodbGetItem /", err)
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve list of game channel
	fmt.Printf("Invite %s by %s: get list of channel", target.User.Username, requester.User.Username)
	serverChannelIDs, err := internal.DynamodbScanAttr(bot.InstanceTable, "key")
	if err != nil {
		fmt.Println("error DynamodbScanAttr /", err)
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
	err := internal.DynamodbScanFindFirst(bot.InstanceTable, "taskArn", *task.TaskArn, &inst)
	if err != nil {
		fmt.Println("error DynamodbGetItem /", err)
		return
	}

	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		fmt.Println("error DynamodbGetItem /", err)
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
