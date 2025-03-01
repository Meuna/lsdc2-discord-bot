package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/meuna/lsdc2-discord-bot/internal"
	"go.uber.org/zap"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
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
		bot.Logger.Error("could not discriminate event type")
	}

	// We make the bot unable to fail: events will never get back to the queue
	return nil
}

func InitBackend() Backend {
	bot, err := internal.InitBot()
	if err != nil {
		panic(err)
	}
	return Backend{bot}
}

type Backend struct {
	internal.BotEnv
}

func (bot Backend) handleSQSEvent(event events.SQSEvent) {
	bot.Logger.Info("received SQS event")

	for _, msg := range event.Records {
		cmd, err := internal.UnmarshallQueuedCmd(msg)
		if err != nil {
			bot.Logger.Error("error in handleSQSEvent",
				zap.String("culprit", "UnmarshallQueuedAction"),
				zap.Any("msg", msg),
				zap.Error(err),
			)
		} else {
			bot.routeFcn(cmd)
		}
	}
}

func (bot Backend) handleCloudWatchEvent(event events.CloudWatchEvent) {
	bot.Logger.Info("received CloudWatch event", zap.String("detailType", event.DetailType))

	switch event.DetailType {
	case "ECS Task State Change":
		bot.notifyTaskUpdate(event)
	default:
		bot.Logger.Error("event not handled", zap.String("event", event.DetailType))
	}
}

//
//	Bot reply
//

func (bot Backend) message(channelID string, msg string, fmtarg ...interface{}) {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		bot.Logger.Error("error in message", zap.String("culprit", "discordgo.New"), zap.Error(err))
		return
	}

	_, err = sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: fmt.Sprintf(msg, fmtarg...),
	})
	if err != nil {
		bot.Logger.Error("error in message", zap.String("culprit", "ChannelMessageSendComplex"), zap.Error(err))
		return
	}
}

func (bot Backend) followUp(cmd internal.BackendCmd, msg string, fmtarg ...interface{}) {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		bot.Logger.Error("error in followUp", zap.String("culprit", "discordgo.New"), zap.Error(err))
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
		bot.Logger.Error("error in followUp", zap.String("culprit", "InteractionResponseEdit"), zap.Error(err))
		return
	}
}

//
//	Backend commands
//

func (bot Backend) routeFcn(cmd internal.BackendCmd) {
	bot.Logger.Debug("routing command", zap.Any("cmd", cmd))

	switch cmd.Api {
	case internal.RegisterGameAPI:
		bot.registerGame(cmd)

	case internal.WelcomeAPI:
		bot.welcomeGuild(cmd)

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
		bot.Logger.Error("unrecognized function", zap.String("action", cmd.Api))
	}
}

//	Game registering

func (bot Backend) registerGame(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.RegisterGameArgs)
	bot.Logger.Debug("received game register request", zap.Any("args", args))

	spec, err := bot._getSpec(cmd, args)
	if err != nil {
		bot.Logger.Error("error in registerGame", zap.String("culprit", "_getSpec"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Check spec is not missing any mandatory field
	missingFields := spec.MissingField()
	if len(missingFields) > 0 {
		bot.Logger.Error("register game is missing fields", zap.Strings("missingFields", missingFields))
		bot.followUp(cmd, "🚫 Spec if missing field %s", missingFields)
		return
	}

	// Check existing spec and abort/cleanup if necessary
	bot.Logger.Debug("registering game: scan game list", zap.String("gameName", spec.Name))
	gameList, err := internal.DynamodbScanAttr(bot.SpecTable, "key")
	if err != nil {
		bot.Logger.Error("error in registerGame", zap.String("culprit", "DynamodbScanAttr"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if internal.Contains(gameList, spec.Name) && !args.Overwrite {
		bot.Logger.Info("game already registered", zap.String("gameName", spec.Name))
		bot.followUp(cmd, "🚫 Game %s already registered and overwrite=False", spec.Name)
		return
	}

	// Security group creation
	bot.Logger.Debug("registering game: ensure previous security group deletion", zap.String("gameName", spec.Name))
	if err = internal.EnsureAndWaitSecurityGroupDeletion(spec.Name, bot.Lsdc2Stack); err != nil {
		bot.Logger.Error("error in registerGame", zap.String("culprit", "EnsureAndWaitSecurityGroupDeletion"), zap.Error(err))
	}
	bot.Logger.Debug("registering game: create security group", zap.String("gameName", spec.Name))
	sgID, err := internal.CreateSecurityGroup(spec, bot.Lsdc2Stack)
	if err != nil {
		bot.Logger.Error("error in registerGame", zap.String("culprit", "CreateSecurityGroup"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	spec.SecurityGroup = sgID

	// Finally, persist the spec in db
	bot.Logger.Debug("registering game: persist spec", zap.String("gameName", spec.Name))
	err = internal.DynamodbPutItem(bot.SpecTable, spec)
	if err != nil {
		bot.Logger.Error("error in registerGame", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
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
		bot.Logger.Debug("registerting: spec download", zap.String("specUrl", args.SpecUrl))
		resp, err = http.Get(args.SpecUrl)
		if err != nil {
			err = fmt.Errorf("http.Get / %w", err)
			return
		}
		defer resp.Body.Close()

		jsonSpec, err = io.ReadAll(resp.Body)
		if err != nil {
			err = fmt.Errorf("io.ReadAll / %w", err)
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
	bot.Logger.Debug("registerting: parse spec")
	if err = json.Unmarshal(jsonSpec, &spec); err != nil {
		err = fmt.Errorf("json.Unmarshal / %w", err)
		return
	}

	return
}

//	Game spinup

func (bot Backend) spinupServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.SpinupArgs)
	bot.Logger.Debug("received server spinup request", zap.Any("args", args))

	// Get spec
	spec, err := bot._getSpecAndIncreaseCount(cmd, args)
	if err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "_getSpecAndIncreaseCount"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	instName := fmt.Sprintf("%s-%d", args.GameName, spec.ServerCount)
	taskFamily := fmt.Sprintf("lsdc2-%s-%s", args.GuildID, instName)

	// Create server channel
	chanID, err := bot._createServerChannel(cmd, args, instName)
	if err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "_createServerChannel"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Register ECS task
	bot.Logger.Debug("spinupServer: register ECS task", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	if spec.EnvMap == nil {
		spec.EnvMap = map[string]string{}
	}
	spec.EnvMap["LSDC2_BUCKET"] = bot.SaveGameBucket
	spec.EnvMap["LSDC2_KEY"] = instName
	for key, value := range args.Env {
		spec.EnvMap[key] = value
	}
	if err = internal.RegisterTask(taskFamily, spec, bot.Lsdc2Stack); err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "RegisterTask"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// And register instance
	bot.Logger.Debug("spinupServer: register instance", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	inst := internal.ServerInstance{
		GuildID:       args.GuildID,
		Name:          instName,
		SpecName:      spec.Name,
		ChannelID:     chanID,
		TaskFamily:    taskFamily,
		SecurityGroup: spec.SecurityGroup,
	}
	if err = internal.DynamodbPutItem(bot.InstanceTable, inst); err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s server creation done !", args.GameName)
}

func (bot Backend) _getSpecAndIncreaseCount(cmd internal.BackendCmd, args internal.SpinupArgs) (spec internal.ServerSpec, err error) {
	bot.Logger.Debug("spinupServer: get spec", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	if err = internal.DynamodbGetItem(bot.SpecTable, args.GameName, &spec); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %w", err)
		return
	}
	if spec.Name == "" {
		err = fmt.Errorf("missing spec")
		return
	}

	bot.Logger.Debug("spinupServer: increment spec count", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	spec.ServerCount = spec.ServerCount + 1
	if err = internal.DynamodbPutItem(bot.SpecTable, spec); err != nil {
		err = fmt.Errorf("DynamodbPutItem / %w", err)
		return
	}

	return
}

func (bot Backend) _createServerChannel(cmd internal.BackendCmd, args internal.SpinupArgs, instName string) (chanID string, err error) {
	// Retrieve guild conf
	bot.Logger.Debug("spinupServer: get guild conf", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %w", err)
		return
	}

	// Create chan
	sessBot, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		err = fmt.Errorf("discordgo.New / %w", err)
		return
	}
	bot.Logger.Debug("spinupServer: create channel", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	channel, err := sessBot.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     instName,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: gc.ChannelCategoryID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			internal.PrivateChannelOverwrite(args.GuildID),
			internal.ViewHistoryAppcmdOverwrite(gc.AdminRoleID),
			internal.HistoryAppcmdOverwrite(gc.UserRoleID),
		},
	})
	if err != nil {
		err = fmt.Errorf("GuildChannelCreateComplex / %w", err)
		return
	}

	// Setup permissions
	scope := "applications.commands.permissions.update applications.commands.update"
	sessBearer, cleanup, err := internal.BearerSession(bot.ClientID, bot.ClientSecret, scope)
	if err != nil {
		err = fmt.Errorf("BearerSession / %w", err)
		return
	}
	defer cleanup()

	bot.Logger.Debug("spinupServer: setting command rights on channel", zap.String("guildID", args.GuildID))
	guildCmd, err := sessBearer.ApplicationCommands(cmd.AppID, args.GuildID)
	if err != nil {
		err = fmt.Errorf("ApplicationCommands / %w", err)
		return
	}
	extendUserCmdFilter := append(internal.UserCmd, internal.InviteKickCmd...)
	extendUserCmd := internal.CommandsWithNameInList(guildCmd, extendUserCmdFilter)

	err = internal.EnableChannelCommands(sessBearer, cmd.AppID, args.GuildID, channel.ID, extendUserCmd)
	if err != nil {
		err = fmt.Errorf("EnableChannelCommands / %w", err)
		return
	}

	chanID = channel.ID
	return
}

//	Game destroy

func (bot Backend) destroyServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.DestroyArgs)
	bot.Logger.Debug("received server destroy request", zap.Any("args", args))

	bot.Logger.Debug("destroy: get inst", zap.String("channelID", args.ChannelID))
	inst := internal.ServerInstance{}
	if err := internal.DynamodbGetItem(bot.InstanceTable, args.ChannelID, &inst); err != nil {
		bot.Logger.Error("error in destroyServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Check if a task is running in which case abort the server destruction
	if inst.TaskArn != "" {
		task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
		if err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "DescribeTask"), zap.Error(err))
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
		if task != nil {
			taskStatus := internal.GetTaskStatus(task)
			if taskStatus != internal.TaskStopped {
				bot.followUp(cmd, "⚠️ The server is running. Please turn it off and try again")
				return
			}
		}
	}

	err := bot._destroyServerInstance(inst)
	if err != nil {
		bot.Logger.Error("error in destroyServer", zap.String("culprit", "_destroyServerInstance"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ Server destruction done !")
}

func (bot Backend) _destroyServerInstance(inst internal.ServerInstance) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %w", err)
	}

	bot.Logger.Debug("destroy: get guild conf", zap.String("channelID", inst.ChannelID))
	if _, err = sess.ChannelDelete(inst.ChannelID); err != nil {
		return fmt.Errorf("ChannelDelete / %w", err)
	}

	bot.Logger.Debug("destroy: unregister task", zap.String("channelID", inst.ChannelID))
	if err = internal.DeregisterTaskFamily(inst.TaskFamily); err != nil {
		return fmt.Errorf("DeregisterTaskFamiliy / %w", err)
	}

	bot.Logger.Debug("destroy: unregister instance", zap.String("channelID", inst.ChannelID))
	if err = internal.DynamodbDeleteItem(bot.InstanceTable, inst.ChannelID); err != nil {
		return fmt.Errorf("DynamodbDeleteItem / %w", err)
	}

	return nil
}

//	Welcoming

func (bot Backend) welcomeGuild(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.WelcomeArgs)
	bot.Logger.Debug("received welcoming request", zap.Any("args", args))

	// Make sure the guild is not bootstrapped
	bot.Logger.Debug("welcoming: check if guild already exists", zap.String("guildID", args.GuildID))
	gcCheck := internal.GuildConf{}
	if err := internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gcCheck); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if gcCheck.GuildID != "" {
		bot.Logger.Info("guild already have an entry in guild table", zap.String("guildID", args.GuildID))
		bot.followUp(cmd, "🚫 Guild appears having already welcomed the bot")
		return
	}

	// Command registering
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "discordgo.New"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	bot.Logger.Debug("welcoming: register commands", zap.String("guildID", args.GuildID))
	if err := internal.CreateGuildsCommands(sess, cmd.AppID, args.GuildID); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "CreateGuildsCommands"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	gc := internal.GuildConf{
		GuildID: args.GuildID,
	}
	if err := bot._createRoles(cmd, args, &gc); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "_createRoles"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._createChannels(cmd, args, &gc); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "_createChannels"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._setupPermissions(cmd, args, gc); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "_setupPermissions"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Register conf
	bot.Logger.Debug("welcoming: register instance", zap.String("guildID", args.GuildID))
	if err := internal.DynamodbPutItem(bot.GuildTable, gc); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
	}

	bot.followUp(cmd, "✅ Welcome complete !")
}

func (bot Backend) _createRoles(cmd internal.BackendCmd, args internal.WelcomeArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %w", err)
	}

	bot.Logger.Debug("welcoming: create LSDC2 roles", zap.String("guildID", args.GuildID))
	adminRole, err := sess.GuildRoleCreate(args.GuildID, &discordgo.RoleParams{
		Name:        "LSDC2 Admin",
		Color:       internal.Pointer(0x8833ff),
		Hoist:       internal.Pointer(true),
		Mentionable: internal.Pointer(true),
	})
	if err != nil {
		return fmt.Errorf("GuildRoleCreate / %w", err)
	}
	userRole, err := sess.GuildRoleCreate(args.GuildID, &discordgo.RoleParams{
		Name:        "LSDC2 User",
		Color:       internal.Pointer(0x33aaff),
		Hoist:       internal.Pointer(true),
		Mentionable: internal.Pointer(true),
	})
	if err != nil {
		return fmt.Errorf("GuildRoleCreate / %w", err)
	}

	gc.AdminRoleID = adminRole.ID
	gc.UserRoleID = userRole.ID

	return nil
}

func (bot Backend) _createChannels(cmd internal.BackendCmd, args internal.WelcomeArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %w", err)
	}

	bot.Logger.Debug("welcoming: create LSDC2 category", zap.String("guildID", args.GuildID))
	lsdc2Category, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name: "LSDC2",
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex / %w", err)
	}
	bot.Logger.Debug("welcoming: create admin LSDC2 channel", zap.String("guildID", args.GuildID))
	adminChan, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     "administration",
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: lsdc2Category.ID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			internal.PrivateChannelOverwrite(args.GuildID),
			internal.ViewHistoryAppcmdOverwrite(gc.AdminRoleID),
		},
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex / %w", err)
	}
	bot.Logger.Debug("welcoming: create welcom LSDC2 channel", zap.String("guildID", args.GuildID))
	welcomeChan, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     "welcome",
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: lsdc2Category.ID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			internal.ViewHistoryInviteOverwrite(gc.AdminRoleID),
		},
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex / %w", err)
	}

	gc.ChannelCategoryID = lsdc2Category.ID
	gc.AdminChannelID = adminChan.ID
	gc.WelcomeChannelID = welcomeChan.ID

	return nil
}

func (bot Backend) _setupPermissions(cmd internal.BackendCmd, args internal.WelcomeArgs, gc internal.GuildConf) error {
	scope := "applications.commands.permissions.update applications.commands.update"
	sess, cleanup, err := internal.BearerSession(bot.ClientID, bot.ClientSecret, scope)
	if err != nil {
		return fmt.Errorf("BearerSession / %w", err)
	}
	defer cleanup()

	bot.Logger.Debug("welcoming: setting commands rights", zap.String("guildID", args.GuildID))
	registeredCmd, err := sess.ApplicationCommands(cmd.AppID, args.GuildID)
	if err != nil {
		return fmt.Errorf("discordgo.ApplicationCommands / %w", err)
	}

	adminCmd := internal.CommandsWithNameInList(registeredCmd, internal.AdminCmd)
	userCmd := internal.CommandsWithNameInList(registeredCmd, internal.UserCmd)

	err = internal.SetupAdminCommands(sess, cmd.AppID, args.GuildID, gc, adminCmd)
	if err != nil {
		return fmt.Errorf("SetupAdminCommands / %w", err)
	}
	err = internal.SetupUserCommands(sess, cmd.AppID, args.GuildID, gc, userCmd)
	if err != nil {
		return fmt.Errorf("SetupUserCommands / %w", err)
	}

	return nil
}

//  Goodbyeing

func (bot Backend) goodbyeGuild(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.GoodbyeArgs)
	bot.Logger.Debug("received goodbye request", zap.Any("args", args))

	// Make sure the guild is bootstrapped
	bot.Logger.Debug("goodbyeing: check if guild exists", zap.String("guildID", args.GuildID))
	gc := internal.GuildConf{}
	if err := internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if gc.GuildID == "" {
		bot.Logger.Info("guild does not have any entry in guild table", zap.String("guildID", args.GuildID))
		bot.followUp(cmd, "🚫 Guild not seems to have welcomed the bot yet")
		return
	}

	// Destroying guild games
	bot.Logger.Debug("goodbyeing: destroying games", zap.String("guildID", args.GuildID))
	err := internal.DynamodbScanDo(bot.InstanceTable, func(inst internal.ServerInstance) (bool, error) {
		if inst.GuildID == args.GuildID {
			if err := bot._destroyServerInstance(inst); err != nil {
				return false, fmt.Errorf("_destroyServerInstance / %w", err)
			}
		}
		return true, nil // keep paging
	})
	if err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DynamodbScanDo"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Command unregistering
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "discordgo.New"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	bot.Logger.Debug("goodbyeing: command deletion", zap.String("guildID", args.GuildID))
	if err := internal.DeleteGuildsCommands(sess, cmd.AppID, args.GuildID); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DeleteGuildsCommands"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if err := bot._deleteChannels(args, &gc); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "_deleteChannels"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._deleteRoles(args, &gc); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "_deleteRoles"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// De-register conf
	bot.Logger.Debug("goodbyeing: deregister instance", zap.String("guildID", args.GuildID))
	if err := internal.DynamodbDeleteItem(bot.GuildTable, gc.GuildID); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DynamodbDeleteItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
	}

	bot.followUp(cmd, "✅ Goodbye complete !")
}

func (bot Backend) _deleteChannels(args internal.GoodbyeArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %w", err)
	}

	bot.Logger.Debug("goodbyeing: LSDC2 category", zap.String("guildID", args.GuildID), zap.String("categoryID", gc.ChannelCategoryID))
	_, err = sess.ChannelDelete(gc.ChannelCategoryID)
	if err != nil {
		return fmt.Errorf("ChannelDelete / %w", err)
	}
	bot.Logger.Debug("goodbyeing: admin channel", zap.String("guildID", args.GuildID), zap.String("channelID", gc.AdminChannelID))
	_, err = sess.ChannelDelete(gc.AdminChannelID)
	if err != nil {
		return fmt.Errorf("ChannelDelete / %w", err)
	}
	bot.Logger.Debug("goodbyeing: welcome channel", zap.String("guildID", args.GuildID), zap.String("channelID", gc.WelcomeChannelID))
	_, err = sess.ChannelDelete(gc.WelcomeChannelID)
	if err != nil {
		return fmt.Errorf("ChannelDelete / %w", err)
	}

	return nil
}

func (bot Backend) _deleteRoles(args internal.GoodbyeArgs, gc *internal.GuildConf) error {
	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		return fmt.Errorf("discordgo.New / %w", err)
	}

	bot.Logger.Debug("goodbyeing: deregistering LSDC2 roles", zap.String("guildID", args.GuildID))
	err = sess.GuildRoleDelete(args.GuildID, gc.AdminRoleID)
	if err != nil {
		return fmt.Errorf("GuildRoleDelete / %w", err)
	}
	err = sess.GuildRoleDelete(args.GuildID, gc.UserRoleID)
	if err != nil {
		return fmt.Errorf("GuildRoleDelete / %w", err)
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
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "discordgo.New"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve requester membership
	bot.Logger.Debug("Invite: retrieve requester member", zap.String("guildID", args.GuildID), zap.String("requesterID", args.RequesterID))
	requester, err := sess.GuildMember(args.GuildID, args.RequesterID)
	if err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "GuildMember"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve target membership
	bot.Logger.Debug("invite: retrieve target member", zap.String("guildID", args.GuildID), zap.String("targetID", args.TargetID))
	target, err := sess.GuildMember(args.GuildID, args.TargetID)
	if err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "GuildMember"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve LSDC2 Admin role from guild conf
	bot.Logger.Debug("Invite: get guild conf",
		zap.String("guildID", args.GuildID),
		zap.String("who", target.User.Username),
		zap.String("by", requester.User.Username),
	)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve list of game channel
	bot.Logger.Debug("Invite: get list of channel",
		zap.String("guildID", args.GuildID),
		zap.String("who", target.User.Username),
		zap.String("by", requester.User.Username),
	)
	serverChannelIDs, err := internal.DynamodbScanAttr(bot.InstanceTable, "key")
	if err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "DynamodbScanAttr"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if internal.Contains(requester.Roles, gc.AdminRoleID) || args.RequesterIsAdmin {
		bot.Logger.Debug("invite: add member role",
			zap.String("guildID", args.GuildID),
			zap.String("who", target.User.Username),
			zap.String("by", requester.User.Username),
		)
		sess.GuildMemberRoleAdd(args.GuildID, args.TargetID, gc.UserRoleID)
		bot.message(gc.WelcomeChannelID, ":call_me: Welcome %s !", target.User.Username)
	} else if !internal.Contains(target.Roles, gc.UserRoleID) {
		bot.followUp(cmd, "🚫 %s is not an allowed LSDC2 user", target.User.Username)
		return
	}
	if internal.Contains(serverChannelIDs, args.ChannelID) {
		bot.Logger.Debug("invite: add member to channel",
			zap.String("guildID", args.GuildID),
			zap.String("who", target.User.Username),
			zap.String("by", requester.User.Username),
		)
		internal.AddUserView(sess, args.ChannelID, args.TargetID)
	}

	bot.followUp(cmd, "%s added !", target.User.Username)
}

func (bot Backend) kickMember(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.KickArgs)

	sess, err := discordgo.New("Bot " + bot.Token)
	if err != nil {
		bot.Logger.Error("error in kickMember", zap.String("culprit", "discordgo.New"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve requester membership
	bot.Logger.Debug("kick: retrieve requester member", zap.String("guildID", args.GuildID), zap.String("requesterID", args.RequesterID))
	requester, err := sess.GuildMember(args.GuildID, args.RequesterID)
	if err != nil {
		bot.Logger.Error("error in kickMember", zap.String("culprit", "GuildMember"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve target membership
	bot.Logger.Debug("kick: retrieve target member", zap.String("guildID", args.GuildID), zap.String("targetID", args.TargetID))
	target, err := sess.GuildMember(args.GuildID, args.TargetID)
	if err != nil {
		bot.Logger.Error("error in kickMember", zap.String("culprit", "GuildMember"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve LSDC2 Admin role from guild conf
	bot.Logger.Debug("kick: get guild conf",
		zap.String("guildID", args.GuildID),
		zap.String("who", target.User.Username),
		zap.String("by", requester.User.Username),
	)
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		bot.Logger.Error("error in kickMember", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Retrieve list of game channel
	bot.Logger.Debug("kick: get list of channel",
		zap.String("guildID", args.GuildID),
		zap.String("who", target.User.Username),
		zap.String("by", requester.User.Username),
	)
	serverChannelIDs, err := internal.DynamodbScanAttr(bot.InstanceTable, "key")
	if err != nil {
		bot.Logger.Error("error in kickMember", zap.String("culprit", "DynamodbScanAttr"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	if internal.Contains(requester.Roles, gc.AdminRoleID) || args.RequesterIsAdmin {
		if args.ChannelID == gc.AdminChannelID {
			bot.Logger.Debug("kick: remove member role",
				zap.String("guildID", args.GuildID),
				zap.String("who", target.User.Username),
				zap.String("by", requester.User.Username),
			)
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
	bot.Logger.Debug("received ECS task state")

	task := ecs.Task{}
	json.Unmarshal(event.Detail, &task)

	inst := internal.ServerInstance{}
	err := internal.DynamodbScanFindFirst(bot.InstanceTable, "taskArn", *task.TaskArn, &inst)
	if err != nil {
		bot.Logger.Error("error in notifyTaskUpdate", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		bot.message(inst.ChannelID, "🚫 Notification error")
		return
	}

	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		bot.Logger.Error("error in notifyTaskUpdate", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.message(inst.ChannelID, "🚫 Notification error")
		return
	}

	switch internal.GetTaskStatus(&task) {
	case internal.TaskStarting:
		bot.message(inst.ChannelID, "📢 Server task state: %s", *task.LastStatus)
	case internal.TaskRunning:
		ip, err := internal.GetTaskIP(&task)
		if err != nil {
			ip = "error retrieving ip"
		}
		bot.message(inst.ChannelID, "✅ Server online at %s (open ports: %s)", ip, spec.OpenPorts())
	case internal.TaskStopping:
		bot.message(inst.ChannelID, "📢 Server task is going offline")
	case internal.TaskStopped:
		bot.message(inst.ChannelID, "📢 Server task went offline")
		bot.Logger.Debug("notify: flag instance as definitely down", zap.String("channelID", inst.ChannelID))
		inst.TaskArn = ""
		if err = internal.DynamodbPutItem(bot.InstanceTable, inst); err != nil {
			bot.Logger.Error("error in stopServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
			bot.message(inst.ChannelID, "🚫 Notification error")
		}
	}
}
