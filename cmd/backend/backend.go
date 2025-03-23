package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/meuna/lsdc2-discord-bot/internal"
	"go.uber.org/zap"

	"maps"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	ecsType "github.com/aws/aws-sdk-go-v2/service/ecs/types"
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

// handleRequest processes incoming SQS and CloudWatch events and
// routes them to the appropriate handler.
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

//===== Section: CloudWatch route

// handleCloudWatchEvent handles incoming CloudWatch events and routes
// them based on the event type.
func (bot Backend) handleCloudWatchEvent(event events.CloudWatchEvent) {
	bot.Logger.Info("received CloudWatch event", zap.String("detailType", event.DetailType))
	bot.Logger.Debug("CloudWatch event", zap.Any("event", event))

	switch event.DetailType {
	case "ECS Task State Change":
		bot.notifyTaskUpdate(event)
	default:
		bot.Logger.Error("event not handled", zap.String("event", event.DetailType))
	}
}

// notifyTaskUpdate handles the notification of ECS task state updates
// and sends appropriate messages based on the task status.
func (bot Backend) notifyTaskUpdate(event events.CloudWatchEvent) {
	bot.Logger.Debug("received ECS task state")
	task := ecsType.Task{}
	json.Unmarshal(event.Detail, &task)

	// Retrieve server instance details
	inst, err := internal.DynamodbScanFindFirst[internal.ServerInstance](bot.InstanceTable, "taskArn", *task.TaskArn)
	if err != nil {
		bot.Logger.Error("error in notifyTaskUpdate", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		bot.message(inst.ChannelID, "🚫 Notification error")
		return
	}

	// Send a message depending on the task status
	switch internal.GetTaskStatus(&task) {
	case internal.TaskStarting:
		bot.message(inst.ThreadID, "📢 Task state: %s", *task.LastStatus)
	case internal.TaskRunning:
		// Get running details: IP
		ip, err := internal.GetTaskIP(&task)
		if err != nil {
			ip = "error retrieving ip"
		}
		// Get spec details: ports
		spec := internal.ServerSpec{}
		err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
		if err != nil {
			bot.Logger.Error("error in notifyTaskUpdate", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
			bot.message(inst.ChannelID, "🚫 Notification error")
			return
		}
		// Message with everything needed to connect
		bot.renameChannel(inst.ThreadID, "🟢 Instance online: %s", ip)
		bot.message(inst.ThreadID, "✅ Instance online ! ```%s```Open ports: %s", ip, spec.OpenPorts())
	case internal.TaskStopping:
		bot.message(inst.ThreadID, "📢 Task is going offline: %s", *task.LastStatus)
	case internal.TaskStopped:
		bot.renameChannel(inst.ThreadID, "🔴 Instance offline")
		bot.message(inst.ThreadID, "📢 Task is offline")
		bot.Logger.Debug("notify: flag instance as definitely down", zap.String("channelID", inst.ChannelID))
		inst.TaskArn = ""
		inst.ThreadID = ""
		if err = internal.DynamodbPutItem(bot.InstanceTable, inst); err != nil {
			bot.Logger.Error("error in stopServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
			bot.message(inst.ChannelID, "🚫 Notification error")
		}
	}
}

//===== Section: SQS route

// handleSQSEvent loop though all received SQS events, unmarshall them
// back into BackendCmd and handle each of them
func (bot Backend) handleSQSEvent(event events.SQSEvent) {
	bot.Logger.Info("received SQS events")
	bot.Logger.Debug("CloudWatch event", zap.Any("event", event))

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

// routeFcn routes the given BackendCmd to the appropriate handler function
// based on the Api field of the command.
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

	case internal.ConfAPI:
		bot.confServer(cmd)

	case internal.DestroyAPI:
		bot.destroyServer(cmd)

	case internal.InviteAPI:
		bot.inviteMember(cmd)

	case internal.KickAPI:
		bot.kickMember(cmd)

	case internal.TaskNotifyAPI:
		bot.forwardTaskNotification(cmd)

	default:
		bot.Logger.Error("unrecognized function", zap.String("action", cmd.Api))
	}
}

//===== Section: game registering

// registerGame handles the registration of a new game. This function
// notably creates a security group and persists the spec in DynamoDB.
func (bot Backend) registerGame(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.RegisterGameArgs)
	bot.Logger.Debug("received game register request", zap.Any("args", args))

	// Retrieve ServerSpec from the command
	spec, err := bot._getSpec(args)
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

	// Check existing spec and abort if user didn't set overwrite=true
	bot.Logger.Debug("registering game: get previous spec version", zap.String("gameName", spec.Name))
	previousSpec := internal.ServerSpec{}
	if err = internal.DynamodbGetItem(bot.SpecTable, spec.Name, &previousSpec); err != nil {
		bot.Logger.Error("error in registerGame", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if previousSpec.Name != "" {
		if !args.Overwrite {
			bot.Logger.Info("game already registered", zap.String("gameName", spec.Name))
			bot.followUp(cmd, "🚫 Game %s already registered and overwrite=False", spec.Name)
			return
		}
		spec.ServerCount = previousSpec.ServerCount
	}

	// Create dedicated security group adapted to the spec ports and protocols
	// There are cases where a security group already exists (overwrite or
	// previous partial failure), so we first try to delete such group.
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

// _getSpec returns a ServerSpec based on the incommand command. It
// handles the 2 cases permited by the frontend:
//  1. An URL is provided, in which case the spec is fetched there
//  2. The spec is directly provided from a modal
func (bot Backend) _getSpec(args internal.RegisterGameArgs) (spec internal.ServerSpec, err error) {
	var jsonSpec []byte

	// Dispatch spec source: args.SpecUrl / args.Spec
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

//===== Section: game spinup

// spinupServer handles the creation of a new server instance. This function
// notably creates a dicsord channel with its permissions, an ECS task
// definition and persists the instance in DynamoDB.
func (bot Backend) spinupServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.SpinupArgs)
	bot.Logger.Debug("received server spinup request", zap.Any("args", args))

	// Retrieve ServerSpec from the db
	spec, err := bot._getSpecAndIncreaseCount(args)
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

	// Register ECS task definition
	bot.Logger.Debug("spinupServer: register ECS task", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	if err = bot._registerTask(taskFamily, instName, spec, args.Env); err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "_registerTask"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// And register instance in db
	bot.Logger.Debug("spinupServer: register instance", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	inst := internal.ServerInstance{
		GuildID:    args.GuildID,
		Name:       instName,
		SpecName:   spec.Name,
		ChannelID:  chanID,
		TaskFamily: taskFamily,
	}
	if err = internal.DynamodbPutItem(bot.InstanceTable, inst); err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s server creation done !", args.GameName)
}

// _getSpecAndIncreaseCount retrieves the ServerSpec and increment the
// spec count, for specific usage of server instance creation.
func (bot Backend) _getSpecAndIncreaseCount(args internal.SpinupArgs) (spec internal.ServerSpec, err error) {
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

// _createServerChannel creates a server channel with the proper
// members and command persmissions
func (bot Backend) _createServerChannel(cmd internal.BackendCmd, args internal.SpinupArgs, instName string) (chanID string, err error) {
	// Retrieve guild conf
	bot.Logger.Debug("spinupServer: get guild conf", zap.String("guildID", args.GuildID), zap.String("gameName", args.GameName))
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %w", err)
		return
	}

	// Create the channel, including its membership rights
	sessBot, _ := discordgo.New("Bot " + bot.Token)
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

	// Setup command permissions
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

func (bot Backend) _registerTask(taskFamily string, instName string, spec internal.ServerSpec, confEnv map[string]string) error {
	if spec.EnvMap == nil {
		spec.EnvMap = map[string]string{}
	}
	spec.EnvMap["LSDC2_BUCKET"] = bot.Bucket
	spec.EnvMap["LSDC2_INSTANCE"] = instName
	spec.EnvMap["LSDC2_QUEUE_URL"] = bot.QueueUrl
	maps.Copy(spec.EnvMap, confEnv)
	return internal.RegisterTask(bot.AwsRegion, taskFamily, spec, bot.Lsdc2Stack)
}

//===== Section: game conf

// confServer handles the configuration of an existing server instance.
func (bot Backend) confServer(cmd internal.BackendCmd) {
	// Get the server instance
	args := *cmd.Args.(*internal.ConfArgs)
	bot.Logger.Debug("received server conf request", zap.Any("args", args))

	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, args.ChannelID, &inst)
	if err != nil {
		bot.Logger.Error("error in confServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Get the game spec
	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		bot.Logger.Error("error in confServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// ECS task revisions
	bot.Logger.Debug("confServer: update ECS task", zap.String("guildID", inst.GuildID), zap.String("instance", inst.Name))
	if err = bot._registerTask(inst.TaskFamily, inst.Name, spec, args.Env); err != nil {
		bot.Logger.Error("error in confServer", zap.String("culprit", "RegisterTask"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s server configuration updated ! (require server restart)", inst.Name)
}

//===== Section: game destroy

// destroyServer removes all resources create for a server, except for
// its S3 savegames. The function abort if the server is running.
func (bot Backend) destroyServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.DestroyArgs)
	bot.Logger.Debug("received server destroy request", zap.Any("args", args))

	// Retrieve ServerInstance from the db
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

	// Destroy the server
	err := bot._destroyServerInstance(inst)
	if err != nil {
		bot.Logger.Error("error in destroyServer", zap.String("culprit", "_destroyServerInstance"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ Server destruction done !")
}

// _destroyServerInstance perform the resources removal. This span the
// Discord channel, the ECS task definition and the entry in DynamoDB
func (bot Backend) _destroyServerInstance(inst internal.ServerInstance) (err error) {
	sess, _ := discordgo.New("Bot " + bot.Token)

	bot.Logger.Debug("destroy: delete channel", zap.String("channelID", inst.ChannelID))
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

//===== Section: welcoming

// welcomeGuild creates all the Discord resources needed to run LSDC2 in
// a guild, and persist the guild info in DynamodDB.
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

	// Create guild level commands
	sess, _ := discordgo.New("Bot " + bot.Token)
	bot.Logger.Debug("welcoming: register commands", zap.String("guildID", args.GuildID))
	guildCmd, err := internal.CreateGuildsCommands(sess, cmd.AppID, args.GuildID)
	if err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "CreateGuildsCommands"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	gc := internal.GuildConf{
		GuildID: args.GuildID,
	}
	// Create roles
	if err := bot._createRoles(args, &gc); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "_createRoles"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	// Create channels including roles permissions
	if err := bot._createChannels(args, &gc); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "_createChannels"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if err := bot._setupCommandPermissions(cmd, args, gc, guildCmd); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "_setupPermissions"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Persist guild in db
	bot.Logger.Debug("welcoming: register instance", zap.String("guildID", args.GuildID))
	if err := internal.DynamodbPutItem(bot.GuildTable, gc); err != nil {
		bot.Logger.Error("error in welcomeGuild", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
	}

	bot.followUp(cmd, "✅ Welcome complete !")
}

// _createRoles creates LSDC2 Admin and LSDC2 User roles for the welcoming guild
func (bot Backend) _createRoles(args internal.WelcomeArgs, gc *internal.GuildConf) error {
	sess, _ := discordgo.New("Bot " + bot.Token)

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

// _createChannels creates an LSDC2 channel category, an admin and a welcome
// channel in the welcoming guild
func (bot Backend) _createChannels(args internal.WelcomeArgs, gc *internal.GuildConf) error {
	sess, _ := discordgo.New("Bot " + bot.Token)

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

// _setupCommandPermissions add guild commands permissions to the created
// channels and roles so that:
//  1. Admin can run admin commands in the admin channel
//  2. Users can run server start/stop commands (but in no channel
//     since at this point, no server instance exsist)
func (bot Backend) _setupCommandPermissions(
	cmd internal.BackendCmd,
	args internal.WelcomeArgs,
	gc internal.GuildConf,
	guildCmd []*discordgo.ApplicationCommand,
) error {
	scope := "applications.commands.permissions.update applications.commands.update"
	sess, cleanup, err := internal.BearerSession(bot.ClientID, bot.ClientSecret, scope)
	if err != nil {
		return fmt.Errorf("BearerSession / %w", err)
	}
	defer cleanup()

	bot.Logger.Debug("welcoming: setting commands rights", zap.String("guildID", args.GuildID))

	adminCmd := internal.CommandsWithNameInList(guildCmd, internal.AdminCmd)
	userCmd := internal.CommandsWithNameInList(guildCmd, internal.UserCmd)

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

//===== Section: goodbyeing

// goodbyeGuild removes all resources created to welcome a guild
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
	allInst, err := internal.DynamodbScan[internal.ServerInstance](bot.InstanceTable)
	if err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DynamodbScan"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	for _, inst := range allInst {
		if inst.GuildID == args.GuildID {
			if err := bot._destroyServerInstance(inst); err != nil {
				bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "_destroyServerInstance"), zap.Error(err))
				bot.followUp(cmd, "🚫 Internal error")
				return
			}
		}
	}

	// Guild commands suppressions
	sess, _ := discordgo.New("Bot " + bot.Token)
	bot.Logger.Debug("goodbyeing: command deletion", zap.String("guildID", args.GuildID))
	if err := internal.DeleteGuildsCommands(sess, cmd.AppID, args.GuildID); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DeleteGuildsCommands"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Channels suppression
	if err := bot._deleteChannels(args, &gc); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "_deleteChannels"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	// Roles suppression
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

// _deleteChannels delete the LSDC2 channel category, admin and welcome channels
func (bot Backend) _deleteChannels(args internal.GoodbyeArgs, gc *internal.GuildConf) (err error) {
	sess, _ := discordgo.New("Bot " + bot.Token)

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

// _deleteRoles deletes LSDC2 Admin and LSDC2 User roles
func (bot Backend) _deleteRoles(args internal.GoodbyeArgs, gc *internal.GuildConf) (err error) {
	sess, _ := discordgo.New("Bot " + bot.Token)

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

//===== Section: invite/Kick command

// inviteMember performs various tasks related to user invites, depending
// on the requester/target existing roles and wether the command is run
// from a server channel:
//  1. If the requester is an admin, the target will be assigned the LSDC2
//     User role.
//  2. If the command is run from a server channel, the target may be added
//     to the channel if he already is a LSDC2 User.
//
// As a result, when run by an admin from a server channel, the target will
// both be added to the LSDC2 User and to the channel. When run by an LSDC2
// User from a server channel, the target will only be added to the server
// channel if he already have the LSDC2 User role.
func (bot Backend) inviteMember(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.InviteArgs)

	sess, _ := discordgo.New("Bot " + bot.Token)

	requester, target, gc, err := bot._getRequesterTargetAndGuildData(sess, args.RequesterID, args.TargetID, args.GuildID)
	if err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "_getRequesterTargetAndGuildData"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Early return if the target is already an admin
	if slices.Contains(target.Roles, gc.AdminRoleID) {
		bot.followUp(cmd, "%s is already an admin 😅", target.User.GlobalName)
		return
	}

	// Assign the LSDC2 User role to the target if needed
	targetIsUser := slices.Contains(target.Roles, gc.UserRoleID)
	requesterIsAdmin := args.RequesterIsAdmin || slices.Contains(requester.Roles, gc.AdminRoleID)
	if requesterIsAdmin && !targetIsUser {
		bot.Logger.Debug("invite: add member role",
			zap.String("guildID", args.GuildID),
			zap.String("who", target.User.GlobalName),
			zap.String("by", requester.User.GlobalName),
		)
		sess.GuildMemberRoleAdd(args.GuildID, args.TargetID, gc.UserRoleID)
		bot.message(gc.WelcomeChannelID, "🤙 Welcome %s !", target.User.GlobalName)

		targetIsUser = true
	}

	// Early return if we are in the administration channel, the job is completed
	if args.ChannelID == gc.AdminChannelID {
		bot.followUp(cmd, "✅ %s invite done !", target.User.GlobalName)
		return
	}

	// If we continue, this means that we are may add the user to a server channel

	// First, early return if the target is not a LSDC2 User. This means a
	// non-admin try to invite a non user.
	if !targetIsUser {
		bot.followUp(cmd, "🚫 %s is not an allowed LSDC2 user", target.User.GlobalName)
		return
	}

	// Then, ensure that the channel is a server channel
	if err := bot._ensureChannelIsAServer(args.ChannelID); err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "_ensureChannelIsAServer"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Then, check if the user is not already in the channel
	hasView, err := internal.HasUserView(sess, args.ChannelID, args.TargetID)
	if err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "HasUserView"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if hasView {
		bot.followUp(cmd, "🤔 %s is already in this channel", target.User.GlobalName)
	} else {
		bot.Logger.Debug("invite: add member to channel",
			zap.String("guildID", args.GuildID),
			zap.String("who", target.User.GlobalName),
			zap.String("by", requester.User.GlobalName),
		)
		internal.AddUserView(sess, args.ChannelID, args.TargetID)
		bot.followUp(cmd, "🤙 Welcome %s ! This channel controls a server. "+
			"Run the command /start, wait a few minute and join the server at the "+
			"provided IP and ports", target.User.GlobalName)
	}
}

// kickMember can only be executed by and admin and remove a user from the
// LSC2 roles and/or servers depending on where the command is executed:
//  1. If the command is run from the LSDC2 admin channel, the target is
//     removed from the LSDC2 User role and all server channels.
//  2. If the command is run from a server channel, the target is only
//     removed from the server.
func (bot Backend) kickMember(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.KickArgs)

	sess, _ := discordgo.New("Bot " + bot.Token)

	requester, target, gc, err := bot._getRequesterTargetAndGuildData(sess, args.RequesterID, args.TargetID, args.GuildID)
	if err != nil {
		bot.Logger.Error("error in inviteMember", zap.String("culprit", "_getRequesterTargetAndGuildData"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Early return if the requester is not an admin
	if !args.RequesterIsAdmin && !slices.Contains(requester.Roles, gc.AdminRoleID) {
		bot.followUp(cmd, "🚫 not allowed")
		return
	}

	// Early return if the target is an admin
	if slices.Contains(target.Roles, gc.AdminRoleID) {
		bot.followUp(cmd, "🚫 not allowed, %s is an admin", target.User.GlobalName)
		return
	}

	// Early return if the command is run from the admin channel
	if args.ChannelID == gc.AdminChannelID {
		// A kick command from the admin channel = kick from LSDC2 role ...
		bot.Logger.Debug("kick: remove member role",
			zap.String("guildID", args.GuildID),
			zap.String("who", target.User.GlobalName),
			zap.String("by", requester.User.GlobalName),
		)
		sess.GuildMemberRoleRemove(args.GuildID, args.TargetID, gc.UserRoleID)

		// ... and all servers
		bot.Logger.Debug("kick: get list of channel", zap.String("guildID", args.GuildID))
		allInst, err := internal.DynamodbScan[internal.ServerInstance](bot.InstanceTable)
		if err != nil {
			bot.Logger.Error("error in kickMember", zap.String("culprit", "DynamodbScanAttr"), zap.Error(err))
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
		for _, inst := range allInst {
			err = internal.RemoveUserView(sess, inst.ChannelID, args.TargetID)
			if err != nil {
				bot.Logger.Error("error in kickMember", zap.String("culprit", "RemoveUserView"), zap.Error(err))
				bot.followUp(cmd, "🚫 Internal error")
				return
			}
		}

		bot.message(gc.WelcomeChannelID, "You're out %s, in your face ! 🖕", target.User.GlobalName)
		bot.followUp(cmd, "✅ %s kick done !", target.User.GlobalName)
		return
	}

	// If we continue, this means that we are may kick the user to a server channel

	// First, ensure that the channel is a server channel
	if err := bot._ensureChannelIsAServer(args.ChannelID); err != nil {
		bot.Logger.Error("error in kickMember", zap.String("culprit", "_ensureChannelIsAServer"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Then, check if the user has view in the channel
	hasView, err := internal.HasUserView(sess, args.ChannelID, args.TargetID)
	if err != nil {
		bot.Logger.Error("error in kickMember", zap.String("culprit", "HasUserView"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if !hasView {
		bot.followUp(cmd, "🤔 %s is not in this channel", target.User.GlobalName)
	} else {
		bot.Logger.Debug("invite: kick member to channel",
			zap.String("guildID", args.GuildID),
			zap.String("who", target.User.GlobalName),
			zap.String("by", requester.User.GlobalName),
		)
		internal.RemoveUserView(sess, args.ChannelID, args.TargetID)
		bot.followUp(cmd, "✅ %s kick done !", target.User.GlobalName)
	}
}

// _getRequesterTargetAndGuildData is a simple helper for the invite/kick functions
func (bot Backend) _getRequesterTargetAndGuildData(sess *discordgo.Session, requesterID string, targetID string, guildID string) (
	requester *discordgo.Member,
	target *discordgo.Member,
	gc internal.GuildConf,
	err error,
) {
	// Retrieve requester membership
	bot.Logger.Debug("kick: retrieve requester member", zap.String("guildID", guildID), zap.String("requesterID", requesterID))
	requester, err = sess.GuildMember(guildID, requesterID)
	if err != nil {
		err = fmt.Errorf("GuildMember / %w", err)
		return
	}

	// Retrieve target membership
	bot.Logger.Debug("kick: retrieve target member", zap.String("guildID", guildID), zap.String("targetID", targetID))
	target, err = sess.GuildMember(guildID, targetID)
	if err != nil {
		err = fmt.Errorf("GuildMember / %w", err)
		return
	}

	// Retrieve the guild conf
	bot.Logger.Debug("kick: get guild conf", zap.String("guildID", guildID))
	if err = internal.DynamodbGetItem(bot.GuildTable, guildID, &gc); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %w", err)
		return
	}
	return
}

// _ensureChannelIsAServer returns an error if the channel is not of a server
func (bot Backend) _ensureChannelIsAServer(channelID string) error {
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		return fmt.Errorf("DynamodbGetItem / %w", err)
	}
	if inst.ChannelID != channelID {
		return errors.New("someone managed to run the invite command in a non-game channel")
	}
	return nil
}

//===== Section: invite/Kick command

// forwardTaskNotification message whatever message the task sent to
// the discord thread
func (bot Backend) forwardTaskNotification(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.TaskNotifyArgs)

	// Retrieve server instance details
	inst, err := internal.DynamodbScanFindFirst[internal.ServerInstance](bot.InstanceTable, "name", args.InstanceName)
	if err != nil {
		bot.Logger.Error("error in forwardTaskNotification", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		bot.message(inst.ChannelID, "🚫 Notification error")
		return
	}

	bot.message(inst.ThreadID, "📢 %s", args.Message)
}

//===== Section: bot reply helpers

// message sends the specified  message to the specified channel
func (bot Backend) message(channelID string, msg string, fmtarg ...interface{}) {
	sess, _ := discordgo.New("Bot " + bot.Token)

	_, err := sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: fmt.Sprintf(msg, fmtarg...),
	})
	if err != nil {
		bot.Logger.Error("error in message", zap.String("culprit", "ChannelMessageSendComplex"), zap.Error(err))
		return
	}
}

// followUp sends the specified message to a previously acknowledged interaction
// (Discord replace the "bot thinking ..." by the message)
func (bot Backend) followUp(cmd internal.BackendCmd, msg string, fmtarg ...interface{}) {
	sess, _ := discordgo.New("Bot " + bot.Token)

	itn := discordgo.Interaction{
		AppID: cmd.AppID,
		Token: cmd.Token,
	}
	_, err := sess.InteractionResponseEdit(&itn, &discordgo.WebhookEdit{
		Content: internal.Pointer(fmt.Sprintf(msg, fmtarg...)),
	})
	if err != nil {
		bot.Logger.Error("error in followUp", zap.String("culprit", "InteractionResponseEdit"), zap.Error(err))
		return
	}
}

func (bot Backend) renameChannel(channelID string, name string, fmtarg ...interface{}) {
	sess, _ := discordgo.New("Bot " + bot.Token)
	sess.ChannelEdit(channelID, &discordgo.ChannelEdit{
		Name: fmt.Sprintf(name, fmtarg...),
	})
}

func (bot Backend) deleteChannel(channelID string) {
	sess, _ := discordgo.New("Bot " + bot.Token)
	sess.ChannelDelete(channelID)
}
