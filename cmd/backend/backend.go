package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/meuna/lsdc2-discord-bot/internal"
	"go.uber.org/zap"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
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

	switch event.DetailType {
	case "ECS Task State Change":
		bot.notifyEcsUpdate(event)
	case "EC2 Instance State-change Notification":
		bot.notifyEc2Update(event)
	default:
		bot.Logger.Error("event not handled", zap.String("event", event.DetailType))
	}
}

// notifyEcsUpdate handles the notification of ECS task state updates
// and sends appropriate messages based on the task status.
func (bot Backend) notifyEcsUpdate(event events.CloudWatchEvent) {
	bot.Logger.Debug("received ECS task state event", zap.Any("event", event))
	task := ecsTypes.Task{}
	if err := json.Unmarshal(event.Detail, &task); err != nil {
		bot.Logger.Error("error in notifyEcsUpdate", zap.String("culprit", "Unmarshal"), zap.Error(err))
		return
	}

	// Retrieve instance
	inst := internal.Instance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, *task.TaskArn, &inst)
	if err != nil {
		bot.Logger.Error("error in notifyEcsUpdate", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return
	}

	// Send a message depending on the task status
	switch internal.GetEcsTaskState(task) {
	case internal.InstanceStateStarting:
		bot.message(inst.ThreadID, "📢 Task state: %s", *task.LastStatus)
	case internal.InstanceStateRunning:
		// Get running details: IP
		ip, err := internal.GetEcsTaskIP(task)
		if err != nil {
			bot.Logger.Error("error in notifyEcsUpdate", zap.String("culprit", "GetEcsTaskIP"), zap.Error(err))
			ip = "error retrieving ip"
		}
		// Message with everything needed to connect
		bot.renameChannel(inst.ThreadID, "🟢 Instance online: %s", ip)
		bot.message(inst.ThreadID, "✅ Instance online ! ```%s```Open ports: %s", ip, inst.OpenPorts)
	case internal.InstanceStateStopping:
		bot.message(inst.ThreadID, "📢 Task is going offline: %s", *task.LastStatus)
	case internal.InstanceStateStopped:
		bot.renameChannel(inst.ThreadID, "🔴 Instance offline")
		bot.message(inst.ThreadID, "📢 Task is offline")
		bot.Logger.Info("notify: instance is down, remove instance DB entry", zap.String("EngineID", inst.EngineID))
		if err := inst.DeregisterTaskFamily(bot.BotEnv); err != nil {
			bot.Logger.Error("error in notifyEcsUpdate", zap.String("culprit", "DeregisterTaskFamily"), zap.Error(err))
		}
		if err := internal.DynamodbDeleteItem(bot.InstanceTable, inst.EngineID); err != nil {
			bot.Logger.Error("error in notifyEcsUpdate", zap.String("culprit", "DynamodbDeleteItem"), zap.Error(err))
			return
		}
	}
}

// notifyEc2Update handles the notification of EC2 task state updates
func (bot Backend) notifyEc2Update(event events.CloudWatchEvent) {
	bot.Logger.Debug("received EC2 task state event", zap.Any("event", event))

	// Parse details
	details := struct {
		InstanceId string                     `json:"instance-id"`
		State      ec2Types.InstanceStateName `json:"state"`
	}{}
	if err := json.Unmarshal(event.Detail, &details); err != nil {
		bot.Logger.Error("error in notifyEc2Update", zap.String("culprit", "Unmarshal"), zap.Error(err))
		return
	}

	// Retrieve instance
	inst := internal.Instance{}
	if err := internal.DynamodbGetItem(bot.InstanceTable, details.InstanceId, &inst); err != nil {
		bot.Logger.Error("error in notifyEc2Update", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return
	}

	// Send a message depending on the task status
	switch internal.GetEc2InstanceState(details.State) {
	case internal.InstanceStateStarting:
		bot.message(inst.ThreadID, "📢 VM state: %s", details.State)
	case internal.InstanceStateRunning:
		// Get running details: IP
		vm, err := internal.DescribeInstance(details.InstanceId)
		var ip string
		if err != nil {
			bot.Logger.Error("error in notifyEc2Update", zap.String("culprit", "DescribeInstance"), zap.Error(err))
			ip = "error retrieving ip"
		} else {
			ip = *vm.PublicIpAddress
		}
		// Message with everything needed to connect
		bot.renameChannel(inst.ThreadID, "🟢 Instance online: %s", ip)
		bot.message(inst.ThreadID, "✅ Instance online ! ```%s```Open ports: %s", ip, inst.OpenPorts)
	case internal.InstanceStateStopping:
		bot.message(inst.ThreadID, "📢 VM is going offline: %s", details.State)
	case internal.InstanceStateStopped:
		bot.renameChannel(inst.ThreadID, "🔴 Instance offline")
		bot.message(inst.ThreadID, "📢 VM is offline")
		bot.Logger.Info("notify: instance is down, remove instance DB entry", zap.String("EngineID", inst.EngineID))
		if err := inst.DeregisterTaskFamily(bot.BotEnv); err != nil {
			bot.Logger.Error("error in notifyEc2Update", zap.String("culprit", "DeregisterTaskFamily"), zap.Error(err))
		}
		if err := internal.DynamodbDeleteItem(bot.InstanceTable, inst.EngineID); err != nil {
			bot.Logger.Error("error in notifyEc2Update", zap.String("culprit", "DynamodbDeleteItem"), zap.Error(err))
			return
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
			bot.routeApi(cmd)
		}
	}
}

// routeApi routes the given BackendCmd to the appropriate handler function
// based on the Api field of the command.
func (bot Backend) routeApi(cmd internal.BackendCmd) {
	bot.Logger.Debug("routing command", zap.Any("cmd", cmd))

	switch cmd.Api {
	case internal.RegisterGameAPI:
		bot.registerGame(cmd)

	case internal.RegisterEngineTierAPI:
		bot.registerEngineTier(cmd)

	case internal.WelcomeAPI:
		bot.welcomeGuild(cmd)

	case internal.GoodbyeAPI:
		bot.goodbyeGuild(cmd)

	case internal.SpinupAPI:
		bot.spinupServer(cmd)

	case internal.ConfAPI:
		bot.confServer(cmd)

	case internal.StartAPI:
		bot.startServer(cmd)

	case internal.DestroyAPI:
		bot.destroyServer(cmd)

	case internal.InviteAPI:
		bot.inviteMember(cmd)

	case internal.KickAPI:
		bot.kickMember(cmd)

	case internal.TaskNotifyAPI:
		bot.routeInstanceNotification(cmd)

	default:
		bot.Logger.Error("unrecognized function", zap.String("action", cmd.Api))
	}
}

// routeInstanceNotification message whatever message the task sent to
// the discord thread
func (bot Backend) routeInstanceNotification(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.TaskNotifyArgs)

	// Retrieve instance details
	inst, err := internal.DynamodbScanFindFirst[internal.Instance](bot.InstanceTable, "ServerName", args.ServerName)
	if err != nil {
		bot.Logger.Error("error in forwardTaskNotification", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		return
	}

	if args.Action == "error" {
		bot.message(inst.ThreadID, "🚫 %s @here", args.Message)
	} else if args.Action == "warning" {
		bot.message(inst.ThreadID, "⚠️ %s @here", args.Message)
	} else {
		bot.message(inst.ThreadID, "📢 %s", args.Message)
	}

	// When the server is ready, when using the EC2 engine and if fastboot
	// is configured, restore the baseline EBS performance
	if args.Action == "server-ready" && inst.EngineType == internal.Ec2EngineType {
		// Retrieve the server
		spec := internal.ServerSpec{}
		if err := internal.DynamodbGetItem(bot.ServerSpecTable, inst.SpecName, &spec); err != nil {
			bot.Logger.Error("error in routeInstanceNotification", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
			bot.message(inst.ThreadID, "🚫 Internal error")
			return
		}

		ec2Spec, ok := spec.Engine.(*internal.Ec2Engine)
		if !ok {
			bot.Logger.Error("engine spec is not an EC2 engine", zap.Any("spec", spec))
			bot.message(inst.ThreadID, "🚫 Internal error")
			return
		}

		if ec2Spec.Fastboot {
			if err := internal.RestoreEbsBaseline(inst.EngineID); err != nil {
				bot.Logger.Error("error in routeInstanceNotification", zap.String("culprit", "RestoreEbsBaseline"), zap.Error(err))
				bot.message(inst.ThreadID, "🚫 Internal error")
				return
			}
		}
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
		bot.followUp(cmd, "🚫 Spec is missing field %s", missingFields)
		return
	}

	// Check existing spec and abort if user didn't set overwrite=true
	bot.Logger.Debug("registering game: get previous spec version", zap.String("gameName", spec.Name))
	previousSpec := internal.ServerSpec{}
	if err = internal.DynamodbGetItem(bot.ServerSpecTable, spec.Name, &previousSpec); err != nil {
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
	bot.Logger.Debug("registering game: persist spec", zap.Any("spec", spec))
	err = internal.DynamodbPutItem(bot.ServerSpecTable, spec)
	if err != nil {
		bot.Logger.Error("error in registerGame", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s register done !", spec.Name)
}

// _getSpec returns a ServerSpec parsed from the incommand command
func (bot Backend) _getSpec(args internal.RegisterGameArgs) (spec internal.ServerSpec, err error) {
	var jsonSpec []byte

	if len(args.Spec) == 0 {
		err = fmt.Errorf("spec is empty")
		return

	}
	jsonSpec = []byte(args.Spec)

	if err = json.Unmarshal(jsonSpec, &spec); err != nil {
		err = fmt.Errorf("json.Unmarshal / %w", err)
		return
	}

	return
}

//===== Section: engine tier registering

// registerGame handles the registration of a new game
func (bot Backend) registerEngineTier(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.RegisterEngineTierArgs)
	bot.Logger.Debug("received engine tier register request", zap.Any("args", args))

	// Retrieve list of EngineTier from the command
	tiers, err := bot._getTiers(args)
	if err != nil {
		bot.Logger.Error("error in registerEngineTier", zap.String("culprit", "_getTiers"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Check tiers are not missing any mandatory field
	for _, tier := range tiers {
		missingFields := tier.MissingField()
		if len(missingFields) > 0 {
			bot.Logger.Error("register tier is missing fields", zap.Strings("missingFields", missingFields))
			bot.followUp(cmd, "🚫 Engine tier is missing field %s", missingFields)
			return
		}
	}

	// Persist the tiers in db
	for _, tier := range tiers {
		bot.Logger.Debug("registering engine tier: persist tier", zap.Any("tier", tier))
		err = internal.DynamodbPutItem(bot.EngineTierTable, tier)
		if err != nil {
			bot.Logger.Error("error in registerEngineTier", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
	}

	bot.followUp(cmd, "✅ %d engine tiers register done !", len(tiers))
}

// _getTier returns a list of EngineTier parsed from the incommand command. It
// handles the 2 following cases:
//  1. The JSON provided is a list of EngineTier
//  2. The JSON provided is a single EngineTier
func (bot Backend) _getTiers(args internal.RegisterEngineTierArgs) (tiers []internal.EngineTier, err error) {
	var jsonSpec []byte

	if len(args.Spec) == 0 {
		err = fmt.Errorf("spec is empty")
		return

	}
	jsonSpec = []byte(args.Spec)

	// Tentative 1: with a list of engine tiers
	if err = json.Unmarshal(jsonSpec, &tiers); err != nil {
		// Tentative 2: with a single engine tier
		tier := internal.EngineTier{}
		if errWithSingle := json.Unmarshal(jsonSpec, &tier); errWithSingle != nil {
			// If both fail, we report the list parsing error
			err = fmt.Errorf("json.Unmarshal / %w", err)
		}
		tiers = []internal.EngineTier{tier}
	}

	return
}

//===== Section: server spinup

// spinupServer handles the creation of a new server. This function
// notably creates a dicsord channel with its permissions, an ECS task
// definition and persists the server in DynamoDB.
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

	// Pick a unique server name
	srvName := fmt.Sprintf("%s-%d", args.GameName, spec.ServerCount)

	// Create server channel
	chanID, err := bot._createServerChannel(cmd, args, srvName)
	if err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "_createServerChannel"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// And register server in db
	srv := internal.Server{
		GuildID:   args.GuildID,
		Name:      srvName,
		SpecName:  spec.Name,
		ChannelID: chanID,
		EnvMap:    args.Env,
	}
	bot.Logger.Debug("spinupServer: register server", zap.Any("srv", srv))
	if err = internal.DynamodbPutItem(bot.ServerTable, srv); err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s server creation done !", args.GameName)
}

// _getSpecAndIncreaseCount retrieves the ServerSpec and increment the
// spec count, for specific usage of server creation.
func (bot Backend) _getSpecAndIncreaseCount(args internal.SpinupArgs) (spec internal.ServerSpec, err error) {
	bot.Logger.Debug("spinupServer: get spec", zap.String("gameName", args.GameName))
	if err = internal.DynamodbGetItem(bot.ServerSpecTable, args.GameName, &spec); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %w", err)
		return
	}
	if spec.Name == "" {
		err = fmt.Errorf("missing spec")
		return
	}

	bot.Logger.Debug("spinupServer: increment spec count", zap.Any("spec", spec))
	spec.ServerCount = spec.ServerCount + 1
	if err = internal.DynamodbPutItem(bot.ServerSpecTable, spec); err != nil {
		err = fmt.Errorf("DynamodbPutItem / %w", err)
		return
	}

	return
}

// _createServerChannel creates a server channel with the proper
// members and command persmissions
func (bot Backend) _createServerChannel(cmd internal.BackendCmd, args internal.SpinupArgs, srvName string) (chanID string, err error) {
	// Retrieve guild conf
	bot.Logger.Debug("spinupServer: get guild conf", zap.String("guildID", args.GuildID))
	gc := internal.GuildConf{}
	if err = internal.DynamodbGetItem(bot.GuildTable, args.GuildID, &gc); err != nil {
		err = fmt.Errorf("DynamodbGetItem / %w", err)
		return
	}

	// Create the channel, including its membership rights
	bot.Logger.Debug("spinupServer: create channel", zap.Any("gc", gc), zap.String("srvName", srvName))
	sessBot, _ := discordgo.New("Bot " + bot.Token)
	channel, err := sessBot.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name:     srvName,
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

//===== Section: server conf

// confServer handles the configuration of an existing server.
func (bot Backend) confServer(cmd internal.BackendCmd) {
	// Get the server
	args := *cmd.Args.(*internal.ConfArgs)
	bot.Logger.Debug("received server conf request", zap.Any("args", args))

	srv := internal.Server{}
	if err := internal.DynamodbGetItem(bot.ServerTable, args.ChannelID, &srv); err != nil {
		bot.Logger.Error("error in confServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Get the game spec
	spec := internal.ServerSpec{}
	if err := internal.DynamodbGetItem(bot.ServerSpecTable, srv.SpecName, &spec); err != nil {
		bot.Logger.Error("error in confServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Reset server env
	srv.EnvMap = args.Env
	bot.Logger.Debug("confServer: update server entry", zap.Any("srv", srv))
	if err := internal.DynamodbPutItem(bot.ServerTable, srv); err != nil {
		bot.Logger.Error("error in spinupServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ %s server configuration updated ! (require server restart)", srv.Name)
}

//===== Section: server start

// startServer starts a server for the given channel ID. It performs the
// following steps:
//  1. Retrieves the server details from DynamoDB.
//  2. Verifies that the task is not already running or in the process of
//     starting/stopping.
//  3. Create a dedicated discord thread.
//  4. Starts the server task/instance if it is not already running.
//  5. Add an instance entry in the db.
func (bot Backend) startServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.StartArgs)
	bot.Logger.Debug("received server start request", zap.Any("args", args))

	// Retrieve server details
	srv := internal.Server{}
	err := internal.DynamodbGetItem(bot.ServerTable, args.ChannelID, &srv)
	if err != nil {
		bot.Logger.Error("error in serverConfigurationFrontloop", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if srv.SpecName == "" {
		bot.followUp(cmd, "🚫 Unrecognised server channel")
		return
	}

	// Check if the server is already running
	existingInst, err := internal.DynamodbScanFindFirst[internal.Instance](bot.InstanceTable, "ServerName", srv.Name)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	if existingInst.EngineID != "" {
		instanceState, err := existingInst.GetState(bot.Lsdc2Stack.EcsClusterName)
		if err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "GetState"), zap.Error(err))
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
		// Test for early return cases
		switch instanceState {
		case internal.InstanceStateStopping:
			bot.followUp(cmd, "⚠️ Server is going offline. Please wait and try again")
			return
		case internal.InstanceStateStarting:
			bot.followUp(cmd, "⚠️ Server is starting. Please wait a few minutes")
			return
		case internal.InstanceStateRunning:
			bot.followUp(cmd, "⚠️ Server is already running. Look for connection info in instance thread")
			return
		}
		// No match == we can start a server
	}

	// Retrieve optional engine tier
	engineTier := internal.EngineTier{}
	if args.EngineTier != "" {
		if err := internal.DynamodbGetItem(bot.EngineTierTable, args.EngineTier, &engineTier); err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
	}

	// Start a dedicated thread
	sess, _ := discordgo.New("Bot " + bot.Token)
	thread, err := sess.ThreadStart(args.ChannelID, "🔵 Instance is starting ...", discordgo.ChannelTypeGuildPublicThread, 1440)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "ThreadStart"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Start the task
	inst, err := srv.StartInstance(bot.BotEnv, engineTier)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "StartTask"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Register the thread ID and task ARN in the instance entry
	inst.ThreadID = thread.ID
	bot.Logger.Debug("startServer: persiste instance in db", zap.Any("inst", inst))
	err = internal.DynamodbPutItem(bot.InstanceTable, inst)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	followUp := "Server starting (wait few minutes)"
	if args.EngineTier != "" {
		followUp = fmt.Sprintf("Server starting with engine tier %s (wait few minutes)", args.EngineTier)
	}
	bot.followUp(cmd, followUp)
}

//===== Section: server destroy

// destroyServer removes all resources create for a server, except for
// its S3 savegames. The function abort if the server is running.
func (bot Backend) destroyServer(cmd internal.BackendCmd) {
	args := *cmd.Args.(*internal.DestroyArgs)
	bot.Logger.Debug("received server destroy request", zap.Any("args", args))

	// Retrieve server details
	bot.Logger.Debug("destroy: get srv", zap.String("channelID", args.ChannelID))
	srv := internal.Server{}
	if err := internal.DynamodbGetItem(bot.ServerTable, args.ChannelID, &srv); err != nil {
		bot.Logger.Error("error in destroyServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Check if a task is running in which case abort the server destruction
	inst, err := internal.DynamodbScanFindFirst[internal.Instance](bot.InstanceTable, "ServerName", srv.Name)
	if err != nil {
		bot.Logger.Error("error in destroyServer", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		bot.message(srv.ChannelID, "🚫 Notification error")
		return
	}
	if inst.EngineID != "" {
		instanceState, err := inst.GetState(bot.Lsdc2Stack.EcsClusterName)
		if err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "GetState"), zap.Error(err))
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
		if instanceState != internal.InstanceStateStopped {
			bot.followUp(cmd, "⚠️ The server is running. Please turn it off and try again")
			return
		}
	}

	// Destroy the server
	if err := bot._destroyServer(srv); err != nil {
		bot.Logger.Error("error in destroyServer", zap.String("culprit", "_destroyServer"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	bot.followUp(cmd, "✅ Server destruction done !")
}

// _destroyServer perform the resources removal. This span the
// Discord channel, the ECS task definition and the entry in DynamoDB
func (bot Backend) _destroyServer(srv internal.Server) (err error) {
	sess, _ := discordgo.New("Bot " + bot.Token)

	bot.Logger.Debug("destroy: delete channel", zap.String("channelID", srv.ChannelID))
	if _, err = sess.ChannelDelete(srv.ChannelID); err != nil {
		return fmt.Errorf("ChannelDelete / %w", err)
	}

	bot.Logger.Debug("destroy: unregister server", zap.String("channelID", srv.ChannelID))
	if err = internal.DynamodbDeleteItem(bot.ServerTable, srv.ChannelID); err != nil {
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
	bot.Logger.Debug("welcoming: register server", zap.Any("gc", gc))
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

	bot.Logger.Debug("welcoming: create LSDC2 category")
	lsdc2Category, err := sess.GuildChannelCreateComplex(args.GuildID, discordgo.GuildChannelCreateData{
		Name: "LSDC2",
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		return fmt.Errorf("GuildChannelCreateComplex / %w", err)
	}
	bot.Logger.Debug("welcoming: create admin LSDC2 channel")
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
	bot.Logger.Debug("welcoming: create welcome LSDC2 channel")
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
//     since at this point, no server exist)
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
	bot.Logger.Debug("goodbyeing: retrieving all servers")
	allSrv, err := internal.DynamodbScan[internal.Server](bot.ServerTable)
	if err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DynamodbScan"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	bot.Logger.Debug("goodbyeing: destroying games", zap.Any("allSrv", allSrv))
	for _, srv := range allSrv {
		if srv.GuildID == args.GuildID {
			if err := bot._destroyServer(srv); err != nil {
				bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "_destroyServer"), zap.Error(err))
				bot.followUp(cmd, "🚫 Internal error")
				return
			}
		}
	}

	// Guild commands suppressions
	sess, _ := discordgo.New("Bot " + bot.Token)

	bot.Logger.Debug("goodbyeing: commands deletion")
	if err := internal.DeleteGuildsCommands(sess, cmd.AppID, args.GuildID); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "DeleteGuildsCommands"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// Channels suppression
	bot.Logger.Debug("goodbyeing: channels deletion")
	if err := bot._deleteChannels(args, &gc); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "_deleteChannels"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}
	// Roles suppression
	bot.Logger.Debug("goodbyeing: roles deletion")
	if err := bot._deleteRoles(args, &gc); err != nil {
		bot.Logger.Error("error in goodbyeGuild", zap.String("culprit", "_deleteRoles"), zap.Error(err))
		bot.followUp(cmd, "🚫 Internal error")
		return
	}

	// De-register conf
	bot.Logger.Debug("goodbyeing: deregister guild", zap.String("guildID", args.GuildID))
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
		bot.Logger.Debug("kick: get list of channel")
		allSrv, err := internal.DynamodbScan[internal.Server](bot.ServerTable)
		if err != nil {
			bot.Logger.Error("error in kickMember", zap.String("culprit", "DynamodbScanAttr"), zap.Error(err))
			bot.followUp(cmd, "🚫 Internal error")
			return
		}
		bot.Logger.Debug("kick: kick member from all servers", zap.Any("allSrv", allSrv))
		for _, srv := range allSrv {
			err = internal.RemoveUserView(sess, srv.ChannelID, args.TargetID)
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
	srv := internal.Server{}
	err := internal.DynamodbGetItem(bot.ServerTable, channelID, &srv)
	if err != nil {
		return fmt.Errorf("DynamodbGetItem / %w", err)
	}
	if srv.ChannelID != channelID {
		return errors.New("someone managed to run the invite command in a non-game channel")
	}
	return nil
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
