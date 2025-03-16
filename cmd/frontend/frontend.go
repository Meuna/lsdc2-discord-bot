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

// handleRequest processes incoming Lambda function URL requests and routes them
// to the appropriate handler based on the request path.
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

//===== Section: upload route

//go:embed upload.html
var uploadPage string

// uploadRoute handles the upload route for the bot. It performs the following steps:
//  1. Parses the query parameters from the request.
//  2. Verifies the MAC and TTL to ensure the request is valid.
//  3. Retrieves the server instance from DynamoDB using the channel ID.
//  4. Generates a presigned S3 PUT URL for uploading the save game.
//  5. Renders an HTML page with the presigned URL embedded.
func (bot Frontend) uploadRoute(request events.LambdaFunctionURLRequest) (events.APIGatewayProxyResponse, error) {
	channelID, mac, eol, parts, err := bot._parseQuery(request)
	if err != nil {
		return internal.Error500(), fmt.Errorf("_parseQuery / %w", err)
	}

	// Verify MAC and TTL
	key := []byte(bot.ClientSecret)
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
		return internal.Error500(), fmt.Errorf("instance %s not found", channelID)
	}

	// Presign S3 PUT
	urls, err := internal.PresignMultipartUploadS3Object(bot.Bucket, inst.Name, parts, 5*time.Minute)
	if err != nil {
		return internal.Error500(), fmt.Errorf("PresignGetS3Object / %w", err)
	}

	// Render HTML from go:embed template
	urlsJson, err := json.Marshal(urls)
	if err != nil {
		return internal.Error500(), err
	}
	r := strings.NewReplacer("{{serverName}}", inst.Name, "{{presignedUrls}}", string(urlsJson))
	uploadPageWithPutUrl := r.Replace(uploadPage)

	return internal.Html200(uploadPageWithPutUrl), nil
}

// _parseQuery extracts the query parameters from the given LambdaFunctionURLRequest
func (bot Frontend) _parseQuery(request events.LambdaFunctionURLRequest) (channelID string, mac []byte, eol int64, parts int, err error) {
	missingKeys := []string{}
	channelID, ok := request.QueryStringParameters["channelID"]
	if !ok {
		missingKeys = append(missingKeys, "channelID")
	}
	macStr, ok := request.QueryStringParameters["mac"]
	if !ok {
		missingKeys = append(missingKeys, "mac")
	}
	partsStr, ok := request.QueryStringParameters["parts"]
	if !ok {
		partsStr = "1"
	}
	eolStr, ok := request.QueryStringParameters["eol"]
	if !ok {
		missingKeys = append(missingKeys, "eol")
	}
	if len(missingKeys) > 0 {
		err = fmt.Errorf("missing keys: %s", missingKeys)
		return
	}

	parts, err = strconv.Atoi(partsStr)
	if err != nil {
		return
	}
	eol, err = strconv.ParseInt(eolStr, 10, 64)
	if err != nil {
		return
	}
	mac, err = base64.RawURLEncoding.DecodeString(macStr)

	return
}

//===== Section: Discord route

// discordRoute handles incoming Discord interactions from a Lambda function URL request.
// It verifies the Discord signature, unmarshals the request body into a Discord interaction,
// and routes the interaction based on its type.
//
// Supported interaction types:
// - Ping
// - ApplicationCommand
// - MessageComponent
// - ApplicationCommandAutocomplete
// - ModalSubmit
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

// checkDiscordSignature verifies the signature of a Discord
// request to prove bot private key ownership
func (bot Frontend) checkDiscordSignature(request events.LambdaFunctionURLRequest) bool {
	pkey, _ := hex.DecodeString(bot.Pkey)
	sig, _ := hex.DecodeString(request.Headers["x-signature-ed25519"])
	pl := []byte(request.Headers["x-signature-timestamp"] + request.Body)

	return ed25519.Verify(pkey, pl, sig)
}

// routeCommand routes the incoming Discord ApplicationCommand to the
// appropriate handler based on the command name.
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
	case internal.ConfAPI:
		return bot.serverConfigurationFrontloop(itn.ChannelID)
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
		return bot.savegameUpload(itn, request.RequestContext.DomainName)
	default:
		bot.Logger.Error("unknown command", zap.String("cmd", acd.Name))
		return bot.reply("🚫 I don't understand ¯\\_(ツ)_/¯")
	}
}

// routeMessageComponent routes the incoming Discord MessageComponent
// to the appropriate handler based on command name, as extracted from
// the custom ID.
func (bot Frontend) routeMessageComponent(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.MessageComponentData()

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

// routeModalSubmit routes the incoming Discord ModalSubmit
// to the appropriate handler based on command name, as extracted from
// the custom ID.
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
	case internal.ConfAPI:
		return bot.serverConfigurationCall(itn)
	default:
		bot.Logger.Error("unknown command", zap.String("cmd", cmd.Api))
		return bot.reply("🚫 I don't understand ¯\\_(ツ)_/¯")
	}
}

// routeAutocomplete routes the incoming Discord ApplicationCommandAutocomplete
// to the appropriate handler based on command name.
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

//===== Section: frontend loop (message component and modal roundtrip)

// In this section, "frontend loop" or "frontloop" refer to the fact that the
// frontend may call itself. This is the case if a command returns a
// MessageComponent or Modal interaction: the bot call sequence looks like this:
// 	1. ApplicationCommand handling (frontent)
//  2. MessageComponent/Modal handling (frontend)
//	3. Backend handling
//
// The transmission of intent between the various steps use the BackendCmd structure.
// Between steps 1 and 2, the BackendCmd is marshalled into CustomID, which has a
// 100 bytes length limit. Between step 1/2 and 3, the BackendCmd is is marshalled
// in JSON over an AWS SQS Queue.
//
// Ref: https://discord.com/developers/docs/interactions/message-components#custom-id

// gameRegisterFrontloop is the first function triggered by a game registration
// command. The function branches between 2 cases:
//  1. If the command was send w/o the SpecUrl option, the function returns a
//     modal to directly prompts the user for the ServerSpec.
//  2. Else, skip the frontloop and directly calls the backend service to
//     register the game.
func (bot Frontend) gameRegisterFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()

	// Get command arguments
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

// welcomeGuildFrontloop is the first function triggered by a guild welcoming
// command. The function simply reply with a confirmation modal.
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

// guildGoodbyeFrontloop is the first function triggered by a guild goodbyeing
// command. The function simply reply with a confirmation modal.
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

// serverDestructionFrontloop is the first function triggered by a server
// destruction command. The function performs the following steps, then
// reply with a confirmation modal.
//  1. The server instances details are fetched from the DynamoDB table
//  2. A first check ensure that the instance with the provided server name
//     exists
//  3. Then a check is made to ensure that the server is not currently running
//  4. Finally, a confirmation modal is replied
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

// serverCreationFrontloop is the first function triggered by a server creation
// command. The function fetch the game spec details from DynamoDB table then
// branches between 2 cases:
//  1. If the spec requires parameters (as defined by the EnvParamMap field),
//     the handler reply with a modal to prompt the parameters from the users.
//  2. Else, skip the frontloop and directly calls the backend service to
//     create the server instance.
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
		return bot._configurationModal(cmd, spec)
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

// serverConfigurationFrontloop is the first function triggered by a server conf
// command. The function fetch the game spec details from DynamoDB table then
// branches between 2 cases:
//  1. If the spec requires parameters (as defined by the EnvParamMap field),
//     the handler reply with a modal to prompt the parameters from the users.
//  2. Else, skip the frontloop and reply that the server does not require conf.
func (bot Frontend) serverConfigurationFrontloop(channelID string) (events.APIGatewayProxyResponse, error) {
	// Get the server instance
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in serverConfigurationFrontloop", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Get the game spec
	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		bot.Logger.Error("error in serverConfigurationFrontloop", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	cmd := internal.BackendCmd{
		Args: &internal.ConfArgs{
			ChannelID: channelID,
		},
	}

	if len(spec.EnvParamMap) > 0 {
		// The instance requires variables: reply with a modal (frontloop)
		return bot._configurationModal(cmd, spec)
	} else {
		return bot.reply("⚠️ The server does not have any configuration")
	}
}

// TODO: improve the modal to enable advance configration
func (bot Frontend) _configurationModal(cmd internal.BackendCmd, spec internal.ServerSpec) (events.APIGatewayProxyResponse, error) {
	paramSpec := make(map[string]string, len(spec.EnvParamMap))
	for env, label := range spec.EnvParamMap {
		paramSpec[env] = label
	}
	title := fmt.Sprintf("Configure %s server", spec.Name)
	return bot.modal(cmd, title, paramSpec)
}

//===== Section: backend call

// Functions in this section call the the backend to perform/finalise the
// requested command.

// callBackend queue up a BackendCmd for the backend
func (bot Frontend) callBackend(cmd internal.BackendCmd) error {
	bot.Logger.Debug("calling backend command", zap.Any("cmd", cmd))
	return internal.QueueMarshalledCmd(bot.QueueUrl, cmd)
}

// genericConfirmedCall is used as the step 2 of a frontloop, after the step 1
// replied with a confirmation modal.
//
// Note 1: this function is triggered AFTER user confirmation so the function
// simply call the backend.
//
// Note 2: routing between step 1 and 2 has to be explicitly developped
// (see routeModalSubmit).
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

// gameRegisterCall is used as the step 2 of a frontloop, after the step 1
// replied with a configuration modal for the ServerSpec. The function
// gathers the prompted spec and call the backend.
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

// serverCreationCall is used as the step 2 of a frontloop, after the step 1
// replied with a server configuration modal. The function gathers the prompted
// parameters and call the backend.
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

// serverConfigurationCall is used as the step 2 of a frontloop, after the step 1
// replied with a server configuration modal. The function gathers the prompted
// parameters and call the backend.
func (bot Frontend) serverConfigurationCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()
	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in serverConfigurationCall", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
		bot.reply("🚫 Internal error")
	}
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token

	args := cmd.Args.(*internal.ConfArgs)
	args.Env = make(map[string]string, len(msd.Components))
	for _, item := range msd.Components {
		textInput := item.(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput)
		key := textInput.CustomID
		value := textInput.Value
		args.Env[key] = value
	}
	cmd.Args = args

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in serverConfigurationCall", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

// memberInviteCall calls the backend to handle invite command (no frontloop)
func (bot Frontend) memberInviteCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	requester := itn.Member
	targetID := acd.Options[0].StringValue()

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

// memberKickCall calls the backend to handle kick command (no frontloop)
func (bot Frontend) memberKickCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	requester := itn.Member
	targetID := acd.Options[0].StringValue()

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

//===== Section: frontend commands

// Functions in this section fully implement the requested command at the frontend level.

// startServer starts a server for the given channel ID. It performs the
// following steps:
//  1. Retrieves the server instance details from DynamoDB.
//  2. Verifies that the task is not already running or in the process of
//     starting/stopping.
//  3. Starts the server task if it is not already running.
//  4. Updates the server instance with the new task ARN in DynamoDB.
func (bot Frontend) startServer(channelID string) (events.APIGatewayProxyResponse, error) {
	// Retrieve server instance details
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

	// Start a dedicated thread
	sess, _ := discordgo.New("Bot " + bot.Token)
	thread, err := sess.ThreadStart(channelID, "🔵 Instance is starting ...", discordgo.ChannelTypeGuildPublicThread, 10080)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "ThreadStart"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Start the task
	taskArn, err := internal.StartTask(inst, bot.Lsdc2Stack)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "StartTask"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Register the thread ID and task ARN in the instance entry
	inst.TaskArn = taskArn
	inst.ThreadID = thread.ID
	err = internal.DynamodbPutItem(bot.InstanceTable, inst)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbPutItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("✅ Server starting (wait few minutes)")
}

// stopServer stops the server for the given channel ID. It performs the
// following steps:
//  1. Retrieves the server instance details from DynamoDB.
//  2. Verifies that the task is not already stop.
//  3. If not, issues the stop request.
func (bot Frontend) stopServer(channelID string) (events.APIGatewayProxyResponse, error) {
	// Retrieve server instance details
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in stopServer", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// Check that the task is not already stopped
	if inst.TaskArn == "" {
		return bot.reply("🟥 Server offline")
	} else {
		task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
		if err != nil {
			bot.Logger.Error("error in startServer", zap.String("culprit", "DescribeTask"), zap.Error(err))
			return bot.reply("🚫 Internal error")
		}
		if task != nil && internal.GetTaskStatus(task) == internal.TaskStopped {
			return bot.reply("🟥 Server offline")
		}
	}

	// Issue the task stop request
	bot.Logger.Debug("stoping: stop task", zap.String("channelID", inst.ChannelID))
	if err = internal.StopTask(inst, bot.Lsdc2Stack); err != nil {
		bot.Logger.Error("error in stopServer", zap.String("culprit", "StopTask"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("⚠️ Server is going offline")
}

// serverStatus retrieves the status of the server associated with the
// given channel ID.
func (bot Frontend) serverStatus(channelID string) (events.APIGatewayProxyResponse, error) {
	// Retrieve server instance details
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("⚠️ This should not happen :thinking:. Are you in a server channel ?")
	}

	// Status: offline
	if inst.TaskArn == "" {
		return bot.reply("🟥 Server offline")
	}
	task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "DescribeTask"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Status: changing
	switch internal.GetTaskStatus(task) {
	case internal.TaskStopped:
		return bot.reply("🟥 Server offline")
	case internal.TaskStopping:
		return bot.reply("⚠️ Server is going offline")
	case internal.TaskStarting:
		return bot.reply("⚠️ Server is starting. Please wait a few minutes")
	}

	// Status: online
	ip, err := internal.GetTaskIP(task)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "GetTaskIP"), zap.Error(err))
		return bot.reply(":thinking: Public IP not available, contact administrator")
	}

	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("✅ Server online at %s (open ports: %s)", ip, spec.OpenPorts())
}

// savegameDownload creates a pre-signed URL for the savegame file stored in
// S3, for the given channel ID, and replies with the link.
func (bot Frontend) savegameDownload(channelID string) (events.APIGatewayProxyResponse, error) {
	// Retrieve server instance details
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		bot.Logger.Error("error in savegameDownload", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// Get the presigned URL
	url, err := internal.PresignGetS3Object(bot.Bucket, inst.Name, time.Minute)
	if err != nil {
		bot.Logger.Error("error in savegameDownload", zap.String("culprit", "PresignGetS3Object"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// We don't use the bot.replyLink approach because S3 presigned URL are too long
	return bot.reply("Link to %s savegame: [Download](%s)", inst.Name, url)
}

// savegameUpload creates a link protected with a MAC and TTL, to the "upload"
// route of the bot frontend address. When clicked, the user leaves Discord
// to meet the bot in a web browser (see uploadRoute), with a web page to
// upload a savegame file.
func (bot Frontend) savegameUpload(itn discordgo.Interaction, botDomain string) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	parts := 1
	if len(acd.Options) > 0 {
		parts = int(acd.Options[0].IntValue())
	}

	// Check that we are in a server channel
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, itn.ChannelID, &inst)
	if err != nil {
		bot.Logger.Error("error in savegameUpload", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// Generate a signed url back to the bot
	key := []byte(bot.ClientSecret)
	msg := []byte(itn.ChannelID)
	ttl := 30
	mac, eol := internal.GenMacWithTTL(key, msg, ttl)

	values := url.Values{}
	values.Add("mac", base64.RawURLEncoding.EncodeToString(mac))
	values.Add("eol", fmt.Sprint(eol))
	values.Add("channelID", itn.ChannelID)
	values.Add("parts", fmt.Sprint(parts))

	url := url.URL{
		Scheme:   "https",
		Host:     botDomain,
		Path:     "upload",
		RawQuery: values.Encode(),
	}
	return bot.replyLink(url.String(), fmt.Sprintf("Open %s savegame upload page", inst.Name), "")
}

// Cache of the choices between lambda calls
// A bit hacky but it's a cheap way to avoid a table scan at each call
var __choicesCache []*discordgo.ApplicationCommandOptionChoice

// autocompleteSpinup returns an autocomplete response with the choices of
// registered games. Note that user inputs is completly ignored: it not
// used to filter the choices.
func (bot Frontend) autocompleteSpinup() (events.APIGatewayProxyResponse, error) {
	// Fast-track the cached reply
	if __choicesCache != nil {
		return bot.replyAutocomplete(__choicesCache)
	}

	gameList, err := internal.DynamodbScanAttr(bot.SpecTable, "key")
	if err != nil {
		return internal.Error500(), fmt.Errorf("DynamodbScanAttr / %w", err)
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

//===== Section: bot reply helpers

// ackMessage acknowledge to Discord that the ApplicationCommand is being handled
// (Discord displays "bot thinking ...")
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

// ackComponent acknowledge to Discord that the MessageComponent is being handled
// (Discord displays "bot thinking ...")
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

// reply replies the specified message
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

// reply replies the specified link with a link button
func (bot Frontend) replyLink(url string, label string, msg string) (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
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

// replyAutocomplete replies to an autocomplete request with the
// specified choices
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

// confirm replies with a modal containing a single TextInput with the
// specified message. It does not strictly looks like a confirmation
// modal but this is the closest found as of this commit.
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

// modal replies with a modal containing as many TextInputShort as
// specified by the paramSpec argument.
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

// textPrompt replies with a modal containing a single TextInput.
// It is very similar to bot.confirm: the only difference is that
// this function uses the Placeholder field of the prompt, which
// has a lenght limit (and thus fail on longer confirmation message).
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
