package main

import (
	"context"
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/meuna/lsdc2-discord-bot/internal"
	"go.uber.org/zap"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/bwmarrin/discordgo"
)

func main() {
	ctx := context.Background()
	ctx = context.WithValue(ctx, "bot", InitFrondend())

	lambda.StartWithOptions(handleRequest, lambda.WithContext(ctx))
}

func handleRequest(ctx context.Context, request events.LambdaFunctionURLRequest) (events.APIGatewayProxyResponse, error) {
	bot := ctx.Value("bot").(Frontend)

	if request.RawPath == "/" {
		return bot.discordRoute(request)
	} else if request.RawPath == "/upload" {
		return bot.uploadRoute(request)
	} else {
		return internal.Error404(), nil
	}
}

func InitFrondend() Frontend {
	bot, err := internal.InitBot()
	if err != nil {
		panic(err)
	}
	return Frontend{bot}
}

type Frontend struct {
	internal.BotEnv
}

//
//	Upload route
//

//go:embed upload.html
var uploadPage string

func (bot Frontend) uploadRoute(request events.LambdaFunctionURLRequest) (events.APIGatewayProxyResponse, error) {
	key := []byte(bot.ClientSecret)
	serverName, channelID, mac, eol, err := bot._parseQuery(request)
	if err != nil {
		return internal.Error500(), fmt.Errorf("_parseQuery / %w", err)
	}

	// Verify MAC and TTL
	if !internal.VerifyMacWithTTL(key, []byte(channelID), eol, mac) {
		return internal.Error401("401: MAC verification failed"), nil
	}
	if time.Now().Unix() > eol {
		return internal.Error401("401: MAC verification failed"), nil
	}

	// Retrieve instance
	inst := internal.ServerInstance{}
	err = internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		return internal.Error500(), fmt.Errorf("DynamodbGetItem / %w", err)
	}
	if inst.SpecName == "" {
		return internal.Error500(), fmt.Errorf("instance %w not found", channelID) // FIXME: replace %w with %s
	}

	// Presign S3 PUT
	url, err := internal.PresignPutS3Object(bot.SaveGameBucket, inst.Name, time.Minute)
	if err != nil {
		return internal.Error500(), fmt.Errorf("PresignGetS3Object / %w", err)
	}

	r := strings.NewReplacer("{{serverName}}", serverName, "{{presignedUrl}}", url)
	uploadPageWithPutUrl := r.Replace(uploadPage)

	return internal.Html200(uploadPageWithPutUrl), nil
}

func (bot Frontend) _parseQuery(request events.LambdaFunctionURLRequest) (serverName string, channelID string, mac []byte, eol int64, err error) {
	missingKeys := []string{}
	serverName, ok := request.QueryStringParameters["serverName"]
	if !ok {
		missingKeys = append(missingKeys, "serverName")
	}
	channelID, ok = request.QueryStringParameters["channelID"]
	if !ok {
		missingKeys = append(missingKeys, "channelID")
	}
	eolStr, ok := request.QueryStringParameters["eol"]
	if !ok {
		missingKeys = append(missingKeys, "eol")
	}
	macStr, ok := request.QueryStringParameters["mac"]
	if !ok {
		missingKeys = append(missingKeys, "mac")
	}
	if len(missingKeys) > 0 {
		err = fmt.Errorf("missing keys: %s", missingKeys)
		return
	}

	eol, err = strconv.ParseInt(eolStr, 10, 64)
	if err != nil {
		return
	}
	mac, err = base64.RawURLEncoding.DecodeString(macStr)

	return
}

//
//	Discord route
//

func (bot Frontend) discordRoute(request events.LambdaFunctionURLRequest) (events.APIGatewayProxyResponse, error) {
	if !bot.checkDiscordSignature(request) {
		return internal.Error401(""), errors.New("signature check failed")
	}

	var itn discordgo.Interaction
	if err := itn.UnmarshalJSON([]byte(request.Body)); err != nil {
		return internal.Error500(), fmt.Errorf("UnmarshalJSON / %s", err)
	}

	switch itn.Type {
	case discordgo.InteractionPing:
		bot.Logger.Info("received PING interaction")
		return internal.Json200(`{"type": 1}`), nil

	case discordgo.InteractionApplicationCommand:
		bot.Logger.Info("received application command interaction")
		return bot.routeCommand(itn, request)

	case discordgo.InteractionMessageComponent:
		bot.Logger.Info("received message component interaction")
		return bot.routeMessageComponent(itn)

	case discordgo.InteractionApplicationCommandAutocomplete:
		bot.Logger.Info("received autocomplete interaction")
		return bot.routeAutocomplete(itn)

	case discordgo.InteractionModalSubmit:
		bot.Logger.Info("received modal submit interaction")
		return bot.routeModalSubmit(itn)

	default:
		return internal.Error500(), fmt.Errorf("unknown interaction type %v", itn.Type)
	}
}

func (bot Frontend) checkDiscordSignature(request events.LambdaFunctionURLRequest) bool {
	pkey, _ := hex.DecodeString(bot.Pkey)
	sig, _ := hex.DecodeString(request.Headers["x-signature-ed25519"])
	pl := []byte(request.Headers["x-signature-timestamp"] + request.Body)

	return ed25519.Verify(pkey, pl, sig)
}

func (bot Frontend) routeCommand(itn discordgo.Interaction, request events.LambdaFunctionURLRequest) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	bot.Logger.Debug("routing command", zap.String("cmd", acd.Name))

	switch acd.Name {
	case internal.RegisterGameAPI:
		return bot.gameRegisterFrontloop(itn)
	case internal.WelcomeAPI:
		return bot.welcomeGuildFrontloop(itn)
	case internal.GoodbyeAPI:
		return bot.guildGoodbyeFrontloop(itn)
	case internal.SpinupAPI:
		return bot.serverCreationFrontloop(itn)
	case internal.DestroyAPI:
		return bot.serverDestructionFrontloop(itn)
	case internal.InviteAPI:
		return bot.memberInviteCall(itn)
	case internal.KickAPI:
		return bot.memberKickCall(itn)
	case internal.StartAPI:
		return bot.startServer(itn.ChannelID)
	case internal.StopAPI:
		return bot.stopServer(itn.ChannelID)
	case internal.StatusAPI:
		return bot.serverStatus(itn.ChannelID)
	case internal.DownloadAPI:
		return bot.savegameDownload(itn.ChannelID)
	case internal.UploadAPI:
		return bot.savegameUpload(itn.ChannelID, request.RequestContext.DomainName)
	default:
		bot.Logger.Error("unknown command", zap.String("cmd", acd.Name))
		return bot.reply("🚫 I don't understand ¯\\_(ツ)_/¯")
	}
}

func (bot Frontend) routeMessageComponent(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()

	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in routeModalSubmit", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
		bot.reply("🚫 Internal error")
	}

	bot.Logger.Debug("routing message component", zap.String("action", cmd.Api))

	// No Message Component implemented yet
	switch cmd.Api {
	default:
		bot.Logger.Error("unknown command", zap.String("cmd", cmd.Api))
		return bot.reply("🚫 I don't understand ¯\\_(ツ)_/¯")
	}
}

func (bot Frontend) routeModalSubmit(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()

	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in routeModalSubmit", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
		bot.reply("🚫 Internal error")
	}

	bot.Logger.Debug("routing modal", zap.String("action", cmd.Api))

	switch cmd.Api {
	case internal.WelcomeAPI:
		return bot.genericConfirmedCall(itn)
	case internal.GoodbyeAPI:
		return bot.genericConfirmedCall(itn)
	case internal.DestroyAPI:
		return bot.genericConfirmedCall(itn)
	case internal.RegisterGameAPI:
		return bot.gameRegisterCall(itn)
	case internal.SpinupAPI:
		return bot.serverCreationCall(itn)
	default:
		bot.Logger.Error("unknown command", zap.String("cmd", cmd.Api))
		return bot.reply("🚫 I don't understand ¯\\_(ツ)_/¯")
	}
}

func (bot Frontend) routeAutocomplete(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	bot.Logger.Debug("routing autocomplete", zap.String("cmd", acd.Name))

	switch acd.Name {
	case internal.SpinupAPI:
		return bot.autocompleteSpinup()
	default:
		return internal.Error500(), fmt.Errorf("unexpected autocomplete request for '%s'", acd.Name)
	}
}

//
//	Frontend loop (message component and modal roundtrip)
//

func (bot Frontend) gameRegisterFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()

	// Get chat command arguments
	args := internal.RegisterGameArgs{}
	for _, opt := range acd.Options {
		if opt.Name == internal.RegisterGameAPISpecUrlOpt {
			args.SpecUrl = opt.StringValue()
		} else if opt.Name == internal.RegisterGameAPIOverwriteOpt {
			args.Overwrite = opt.BoolValue()
		} else {
			bot.Logger.Error("unknown option", zap.String("opt", opt.Name))
			return bot.reply("🚫 Internal error")
		}
	}

	if args.SpecUrl == "" {
		// We don't have a spec url: reply with a modal (frontloop)
		cmd := internal.BackendCmd{Args: &args}
		return bot.textPrompt(cmd, "Register new game", "Paste LSDC2 json spec", `{"key": "gamename", "image": "repo/image:tag" ... }`)
	} else {
		// We have a spec url: directly call the backend (skip frontloop)
		cmd := internal.BackendCmd{
			AppID: itn.AppID,
			Token: itn.Token,
			Args:  &args,
		}

		if err := bot.callBackend(cmd); err != nil {
			bot.Logger.Error("error in requestGameRegister", zap.String("culprit", "callBackend"), zap.Error(err))
			return bot.reply("🚫 Internal error")
		}
		return bot.ackMessage()
	}
}

func (bot Frontend) welcomeGuildFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	cmd := internal.BackendCmd{
		Args: &internal.WelcomeArgs{
			GuildID: itn.GuildID,
		},
	}
	title := "Welcome Mr. bot"
	confimationText := "When you click send, you will welcome LSDC2 bot in your guild, including its roles and channels"
	return bot.confirm(cmd, title, confimationText)
}

func (bot Frontend) guildGoodbyeFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	cmd := internal.BackendCmd{
		Args: &internal.GoodbyeArgs{
			GuildID: itn.GuildID,
		},
	}
	title := "Goodbye Mr. bot"
	confimationText := "When you click send, the LSDC2 bot will say goodbye to your guild. You will not be able to retrieve savegames anymore."
	return bot.confirm(cmd, title, confimationText)
}

func (bot Frontend) serverDestructionFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	serverName := acd.Options[0].StringValue()

	// Retrieve the chanel ID
	inst := internal.ServerInstance{}
	err := internal.DynamodbScanFindFirst(bot.InstanceTable, "name", serverName, &inst)
	if err != nil {
		bot.Logger.Error("error in confirmServerDestruction", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.ChannelID == "" {
		return bot.reply("🚫 Server %s not found", serverName)
	}

	// Check if a task is running
	if inst.TaskArn != "" {
		task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
		if err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "DescribeTask"), zap.Error(err))
			return bot.reply("🚫 Internal error")
		}
		if task != nil {
			taskStatus := internal.GetTaskStatus(task)
			if taskStatus != internal.TaskStopped {
				return bot.reply("⚠️ The server is running. Please turn it off and try again")
			}
		}
	}

	cmd := internal.BackendCmd{
		Args: &internal.DestroyArgs{
			ChannelID: inst.ChannelID,
		},
	}
	title := fmt.Sprintf("Delete %s", serverName)
	confimationText := fmt.Sprintf(
		"When you click send, the %s server will be removed from your guild. You will not be able to retrieve savegames anymore.",
		serverName)
	return bot.confirm(cmd, title, confimationText)
}

func (bot Frontend) serverCreationFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	gameName := acd.Options[0].StringValue()

	spec := internal.ServerSpec{}
	err := internal.DynamodbGetItem(bot.SpecTable, gameName, &spec)
	if err != nil {
		bot.Logger.Error("error in configureServerCreation", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if spec.Name == "" {
		return bot.reply("⚠️ Game spec %s not found :thinking:", gameName)
	}

	cmd := internal.BackendCmd{
		Args: &internal.SpinupArgs{
			GameName: gameName,
			GuildID:  itn.GuildID,
		},
	}

	if len(spec.EnvParamMap) > 0 {
		// The instance requires variables: reply with a modal (frontloop)
		paramSpec := make(map[string]string, len(spec.EnvParamMap))
		for env, label := range spec.EnvParamMap {
			paramSpec[env] = label
		}
		title := fmt.Sprintf("Configure %s server", gameName)
		return bot.modal(cmd, title, paramSpec)
	} else {
		// Else directly call the backend (skip frontloop)
		cmd.AppID = itn.AppID
		cmd.Token = itn.Token
		if err := bot.callBackend(cmd); err != nil {
			bot.Logger.Error("error in configureServerCreation", zap.String("culprit", "callBackend"), zap.Error(err))
			return bot.reply("🚫 Internal error")
		}
		return bot.ackMessage()
	}
}

//
//	Backend call
//

func (bot Frontend) callBackend(cmd internal.BackendCmd) error {
	bot.Logger.Debug("calling backend command", zap.Any("cmd", cmd))
	return internal.QueueMarshalledCmd(bot.QueueUrl, cmd)
}

func (bot Frontend) genericConfirmedCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()
	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in genericConfirmedCall", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
		bot.reply("🚫 Internal error")
	}
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in routeMessageComponent", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	return bot.ackMessage()
}

func (bot Frontend) gameRegisterCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()
	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in requestGameRegister", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
		bot.reply("🚫 Internal error")
	}
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token

	item := msd.Components[0]
	textInput := item.(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput)

	args := cmd.Args.(*internal.RegisterGameArgs)
	args.Spec = textInput.Value
	cmd.Args = args

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in requestGameRegister", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

func (bot Frontend) serverCreationCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()
	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in requestServerCreation", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
		bot.reply("🚫 Internal error")
	}
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token

	args := cmd.Args.(*internal.SpinupArgs)
	args.Env = make(map[string]string, len(msd.Components))
	for _, item := range msd.Components {
		textInput := item.(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput)
		key := textInput.CustomID
		value := textInput.Value
		args.Env[key] = value
	}
	cmd.Args = args

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in requestServerCreation", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

func (bot Frontend) memberInviteCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	requester := itn.Member
	targetID := acd.Options[0].Value.(string)

	cmd := internal.BackendCmd{
		AppID: itn.AppID,
		Token: itn.Token,
		Args: &internal.InviteArgs{
			GuildID:          itn.GuildID,
			ChannelID:        itn.ChannelID,
			RequesterID:      requester.User.ID,
			TargetID:         targetID,
			RequesterIsAdmin: internal.IsAdmin(requester),
		},
	}

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in requestMemberInvite", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

func (bot Frontend) memberKickCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	requester := itn.Member
	targetID := acd.Options[0].Value.(string)

	cmd := internal.BackendCmd{
		AppID: itn.AppID,
		Token: itn.Token,
		Args: &internal.KickArgs{
			GuildID:          itn.GuildID,
			ChannelID:        itn.ChannelID,
			RequesterID:      requester.User.ID,
			TargetID:         targetID,
			RequesterIsAdmin: internal.IsAdmin(requester),
		},
	}

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in requestMemberKick", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

//
//	Frontend commands
//

func (bot Frontend) startServer(channelID string) (events.APIGatewayProxyResponse, error) {
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Unrecognised server channel")
	}

	// Check that the task is not yet running
	if inst.TaskArn != "" {
		task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
		if err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "DescribeTask"), zap.Error(err))
			return bot.reply("🚫 Internal error")
		}
		if task != nil {
			switch internal.GetTaskStatus(task) {
			case internal.TaskStopping:
				return bot.reply("⚠️ Server is going offline. Please wait and try again")
			case internal.TaskStarting:
				return bot.reply("⚠️ Server is starting. Please wait a few minutes")
			case internal.TaskRunning:
				return bot.serverStatus(channelID)
			}
			// No match == we can start a server
		}
	}

	taskArn, err := internal.StartTask(inst, bot.Lsdc2Stack)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "StartTask"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	inst.TaskArn = taskArn
	err = internal.DynamodbPutItem(bot.InstanceTable, inst)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("✅ Server starting (wait few minutes)")
}

func (bot Frontend) stopServer(channelID string) (events.APIGatewayProxyResponse, error) {
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in stopServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// Check that the task is not yet running
	if inst.TaskArn == "" {
		return bot.reply("🟥 Server offline")
	} else {
		task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
		if err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "DescribeTask"), zap.Error(err))
			return bot.reply("🚫 Internal error")
		}
		if task != nil {
			switch internal.GetTaskStatus(task) {
			case internal.TaskStopped:
				return bot.reply("🟥 Server offline")
			}
			// No match == we can issue a stop command
		}
	}

	bot.Logger.Debug("stoping: stop task", zap.String("channelID", inst.ChannelID))
	if err = internal.StopTask(inst, bot.Lsdc2Stack); err != nil {
		bot.Logger.Error("error in stopServer", zap.String("culprit", "StopTask"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("⚠️ Server is going offline")
}

func (bot Frontend) serverStatus(channelID string) (events.APIGatewayProxyResponse, error) {
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("⚠️ This should not happen :thinking:. Are you in a server channel ?")
	}

	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	if inst.TaskArn == "" {
		return bot.reply("🟥 Server offline")
	}
	task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "DescribeTask"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	switch internal.GetTaskStatus(task) {
	case internal.TaskStopped:
		return bot.reply("🟥 Server offline")
	case internal.TaskStopping:
		return bot.reply("⚠️ Server is going offline")
	case internal.TaskStarting:
		return bot.reply("⚠️ Server is starting. Please wait a few minutes")
	}

	ip, err := internal.GetTaskIP(task)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "GetTaskIP"), zap.Error(err))
		return bot.reply(":thinking: Public IP not available, contact administrator")
	}
	return bot.reply("✅ Server online at %s (open ports: %s)", ip, spec.OpenPorts())
}

func (bot Frontend) savegameDownload(channelID string) (events.APIGatewayProxyResponse, error) {
	// Check that we are in a server channel
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in savegameDownload", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	url, err := internal.PresignGetS3Object(bot.SaveGameBucket, inst.Name, time.Minute)
	if err != nil {
		bot.Logger.Error("error in savegameDownload", zap.String("culprit", "PresignGetS3Object"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("Link to %s savegame: [Download](%s)", inst.Name, url)
}

func (bot Frontend) savegameUpload(channelID string, domainName string) (events.APIGatewayProxyResponse, error) {
	// Check that we are in a server channel
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in savegameUpload", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// And generate a signed url back to the bot
	key := []byte(bot.ClientSecret)
	msg := []byte(channelID)
	ttl := 30
	mac, eol := internal.GenMacWithTTL(key, msg, ttl)

	values := url.Values{}
	values.Add("mac", base64.RawURLEncoding.EncodeToString(mac))
	values.Add("eol", fmt.Sprint(eol))
	values.Add("channelID", channelID)
	values.Add("serverName", inst.Name)

	url := url.URL{
		Scheme:   "https",
		Host:     domainName,
		Path:     "upload",
		RawQuery: values.Encode(),
	}
	return bot.replyLink(url.String(), "Open upload page", "%s savegame", inst.Name)
}

// Cache of the choices between lambda calls
// A bit hacky but it's a cheap way to avoid a table scan at each call
var __choicesCache []*discordgo.ApplicationCommandOptionChoice

func (bot Frontend) autocompleteSpinup() (events.APIGatewayProxyResponse, error) {
	// Fast-track the cached reply
	if __choicesCache != nil {
		return bot.replyAutocomplete(__choicesCache)
	}

	gameList, err := internal.DynamodbScanAttr(bot.SpecTable, "key")
	if err != nil {
		return internal.Error500(), fmt.Errorf("DynamodbScanAttr / %s", err) // FIXME: change %s to %w
	}

	choices := make([]*discordgo.ApplicationCommandOptionChoice, len(gameList))
	for idx, item := range gameList {
		choices[idx] = &discordgo.ApplicationCommandOptionChoice{
			Name:  item, // This is the value displayed to the user
			Value: item, // This is the value sent to the command
		}
	}

	__choicesCache = choices

	return bot.replyAutocomplete(choices)
}

//
//	Bot reply helpers
//

func (bot Frontend) ackMessage() (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		bot.Logger.Error("error in ackMessage", zap.String("culprit", "marshal"), zap.Error(err))
		return internal.Error500(), err
	}
	return internal.Json200(string(jsonBytes[:])), nil
}

func (bot Frontend) ackComponent() (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		bot.Logger.Error("error in ackComponent", zap.String("culprit", "marshal"), zap.Error(err))
		return internal.Error500(), err
	}
	return internal.Json200(string(jsonBytes[:])), nil
}

func (bot Frontend) reply(msg string, fmtarg ...interface{}) (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf(msg, fmtarg...),
		},
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		bot.Logger.Error("error in reply", zap.String("culprit", "marshal"), zap.Error(err))
		return internal.Error500(), fmt.Errorf("marshal / %s", err)
	}
	return internal.Json200(string(jsonBytes[:])), nil
}

func (bot Frontend) replyLink(url string, label string, msg string, fmtarg ...interface{}) (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf(msg, fmtarg...),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.Button{
							Label: label,
							Style: discordgo.LinkButton,
							URL:   url,
						},
					},
				},
			},
		},
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return internal.Error500(), fmt.Errorf("marshal / %s", err)
	}
	return internal.Json200(string(jsonBytes[:])), nil
}

func (bot Frontend) replyAutocomplete(choices []*discordgo.ApplicationCommandOptionChoice) (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{
			Choices: choices,
		},
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return internal.Error500(), fmt.Errorf("marshal / %s", err)
	}
	return internal.Json200(string(jsonBytes[:])), nil
}

func (bot Frontend) confirm(cmd internal.BackendCmd, title string, msg string) (events.APIGatewayProxyResponse, error) {
	customID, err := internal.MarshalCustomID(cmd)
	if err != nil {
		bot.Logger.Error("error in textPrompt", zap.String("culprit", "MarshalCustomIDAction"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: customID,
			Title:    title,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							Label:     title,
							Value:     msg,
							Style:     discordgo.TextInputParagraph,
							CustomID:  customID,
							MaxLength: 0,
						},
					},
				},
			},
		},
	}

	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return internal.Error500(), fmt.Errorf("marshal / %s", err)
	}
	return internal.Json200(string(jsonBytes[:])), nil
}

func (bot Frontend) modal(cmd internal.BackendCmd, title string, paramSpec map[string]string) (events.APIGatewayProxyResponse, error) {
	customID, err := internal.MarshalCustomID(cmd)
	if err != nil {
		bot.Logger.Error("error in modal", zap.String("culprit", "MarshalCustomIDAction"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	params := make([]discordgo.MessageComponent, len(paramSpec))
	idx := 0
	for key, value := range paramSpec {
		params[idx] = discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.TextInput{
					Label:    value,
					Style:    discordgo.TextInputShort,
					CustomID: key,
				},
			},
		}

		idx = idx + 1
	}

	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID:   customID,
			Title:      title,
			Components: params,
		},
	}

	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return internal.Error500(), fmt.Errorf("marshal / %s", err)
	}
	return internal.Json200(string(jsonBytes[:])), nil
}

func (bot Frontend) textPrompt(cmd internal.BackendCmd, title string, label string, placeholder string) (events.APIGatewayProxyResponse, error) {
	customID, err := internal.MarshalCustomID(cmd)
	if err != nil {
		bot.Logger.Error("error in textPrompt", zap.String("culprit", "MarshalCustomIDAction"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: customID,
			Title:    title,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							Label:       label,
							Placeholder: placeholder,
							Style:       discordgo.TextInputParagraph,
							CustomID:    "text",
							Required:    true,
						},
					},
				},
			},
		},
	}

	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return internal.Error500(), fmt.Errorf("marshal / %s", err)
	}
	return internal.Json200(string(jsonBytes[:])), nil
}
