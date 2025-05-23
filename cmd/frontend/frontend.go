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
	"sort"
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
//  3. Retrieves the server details from DynamoDB using the channel ID.
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

	// Retrieve server details
	srv := internal.Server{}
	err = internal.DynamodbGetItem(bot.ServerTable, channelID, &srv)
	if err != nil {
		return internal.Error500(), fmt.Errorf("DynamodbGetItem / %w", err)
	}
	if srv.SpecName == "" {
		return internal.Error500(), fmt.Errorf("server %s not found", channelID)
	}

	// Presign S3 PUT
	urls, err := internal.PresignMultipartUploadS3Object(bot.Bucket, srv.Name, parts, 5*time.Minute)
	if err != nil {
		return internal.Error500(), fmt.Errorf("PresignGetS3Object / %w", err)
	}

	// Render HTML from go:embed template
	urlsJson, err := json.Marshal(urls)
	if err != nil {
		return internal.Error500(), err
	}
	r := strings.NewReplacer("{{serverName}}", srv.Name, "{{presignedUrls}}", string(urlsJson))
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
	bot.Logger.Debug("Discord interaction", zap.Any("interaction", itn))

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
	bot.Logger.Debug("routing command", zap.Any("acd", acd))

	switch acd.Name {
	case internal.RegisterGameAPI:
		return bot.gameRegisterFrontloop(itn)
	case internal.RegisterEngineTierAPI:
		return bot.engineTierRegisterFrontloop(itn)
	case internal.WelcomeAPI:
		return bot.welcomeGuildFrontloop(itn)
	case internal.GoodbyeAPI:
		return bot.guildGoodbyeFrontloop(itn)
	case internal.SpinupAPI:
		return bot.serverCreationFrontloop(itn)
	case internal.ConfAPI:
		return bot.serverConfigurationFrontloop(itn)
	case internal.StartAPI:
		return bot.serverStartCall(itn)
	case internal.DestroyAPI:
		return bot.serverDestructionFrontloop(itn)
	case internal.InviteAPI:
		return bot.memberInviteCall(itn)
	case internal.KickAPI:
		return bot.memberKickCall(itn)
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

	bot.Logger.Debug("routing message component", zap.String("Api", cmd.Api))

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

	bot.Logger.Debug("routing modal", zap.String("Api", cmd.Api))

	switch cmd.Api {
	case internal.RegisterGameAPI:
		return bot.gameRegisterCall(itn)
	case internal.RegisterEngineTierAPI:
		return bot.engineTierRegisterCall(itn)
	case internal.WelcomeAPI:
		return bot.genericConfirmedCall(itn)
	case internal.GoodbyeAPI:
		return bot.genericConfirmedCall(itn)
	case internal.DestroyAPI:
		return bot.genericConfirmedCall(itn)
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
	bot.Logger.Debug("routing autocomplete", zap.Any("acd", acd))

	switch acd.Name {
	case internal.SpinupAPI:
		return bot.autocompleteSpinup(acd.Options)
	case internal.StartAPI:
		return bot.autocompleteStart(acd.Options)
	default:
		return internal.Error500(), fmt.Errorf("unexpected autocomplete request for '%s'", acd.Name)
	}
}

//===== Section: frontend loop (message component and modal roundtrip)

// In this section, "frontend loop" or "frontloop" refer to the fact that the
// frontend may call itself. This is the case if a command returns a
// MessageComponent or Modal interaction: the bot call sequence looks like this:
// 	1. ApplicationCommand handling (frontend)
//  2. MessageComponent/Modal handling (frontend)
//	3. Backend handling
//
// The transmission of intent between the various steps uses the BackendCmd structure.
// Between steps 1 and 2, the BackendCmd is marshalled into CustomID, which has a
// 100 bytes length limit. Between step 1/2 and 3, the BackendCmd is marshalled
// in JSON over an AWS SQS Queue.
//
// Ref: https://discord.com/developers/docs/interactions/message-components#custom-id

// gameRegisterFrontloop is the first function triggered by a game registration
// command. It returns a modal to prompt the user for the ServerSpec.
func (bot Frontend) gameRegisterFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	// Ensure that the user running the command is the bot owner
	if itn.User.ID != bot.OwnerID {
		return bot.reply("🚫 Internal error")
	}

	acd := itn.ApplicationCommandData()

	args := internal.RegisterGameArgs{}

	// Get command arguments
	if len(acd.Options) > 0 {
		args.Overwrite = acd.Options[0].BoolValue()
	}

	cmd := internal.BackendCmd{Args: &args}
	return bot.textPrompt(cmd, "Register new game", "Paste LSDC2 json spec", `{"name": "gamename", "image": "repo/image:tag" ... }`)
}

// engineTierRegisterFrontloop is the first function triggered by an engine tier
// registration command. It returns a modal to prompt the user for the EngineTier.
func (bot Frontend) engineTierRegisterFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	// Ensure that the user running the command is the bot owner
	if itn.User.ID != bot.OwnerID {
		return bot.reply("🚫 Internal error")
	}

	cmd := internal.BackendCmd{Args: &internal.RegisterEngineTierArgs{}}
	return bot.textPrompt(cmd, "Register engine tier", "Paste LSDC2 json spec", `[{"name": "2c8gb", "cpu": "2 vCPU", ... }, ...]`)
}

// welcomeGuildFrontloop is the first function triggered by a guild welcoming
// command. The function simply replies with a confirmation modal.
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
// command. The function simply replies with a confirmation modal.
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

// serverCreationFrontloop is the first function triggered by a server creation
// command. The function fetches the game spec details from DynamoDB table then
// branches between 2 cases:
//  1. If the spec requires parameters (as defined by the Params field),
//     the handler replies with a modal to prompt the parameters from the users.
//  2. Else, skip the frontloop and directly calls the backend service to
//     create the server.
func (bot Frontend) serverCreationFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	gameName := acd.Options[0].StringValue()

	spec := internal.ServerSpec{}
	err := internal.DynamodbGetItem(bot.ServerSpecTable, gameName, &spec)
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

	if len(spec.Params) > 0 {
		// The server requires variables: reply with a modal (frontloop)
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
// command. The function fetches the game spec details from DynamoDB table then
// branches between 2 cases:
//  1. If the spec requires parameters (as defined by the Params field),
//     the handler replies with a modal to prompt the parameters from the users.
//  2. Else, skip the frontloop and reply that the server does not require conf.
func (bot Frontend) serverConfigurationFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	channelID := acd.Options[0].Value.(string) // We shortcut to the value because discordgo API want to query channel details

	// Get the server details
	srv := internal.Server{}
	err := internal.DynamodbGetItem(bot.ServerTable, channelID, &srv)
	if err != nil {
		bot.Logger.Error("error in serverConfigurationFrontloop", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Get the game spec
	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.ServerSpecTable, srv.SpecName, &spec)
	if err != nil {
		bot.Logger.Error("error in serverConfigurationFrontloop", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	cmd := internal.BackendCmd{
		Args: &internal.ConfArgs{
			ChannelID: channelID,
		},
	}

	if len(spec.Params) > 0 {
		// The server requires variables: reply with a modal (frontloop)
		return bot._configurationModal(cmd, spec)
	} else {
		return bot.reply("⚠️ The server does not have any configuration")
	}
}

// serverDestructionFrontloop is the first function triggered by a server
// destruction command. The function performs the following steps, then
// replies with a confirmation modal:
//  1. The server details are fetched from the DynamoDB table.
//  2. A first check ensures that the server with the provided name exists.
//  3. Then a check is made to ensure that the server is not currently running.
//  4. Finally, a confirmation modal is returned.
func (bot Frontend) serverDestructionFrontloop(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	serverName := acd.Options[0].StringValue()

	// Retrieve the server
	srv, err := internal.DynamodbScanFindFirst[internal.Server](bot.ServerTable, "Name", serverName)
	if err != nil {
		bot.Logger.Error("error in confirmServerDestruction", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if srv.ChannelID == "" {
		return bot.reply("🚫 Server %s not found", serverName)
	}

	cmd := internal.BackendCmd{
		Args: &internal.DestroyArgs{
			ChannelID: srv.ChannelID,
		},
	}
	title := fmt.Sprintf("Delete %s", serverName)
	confimationText := fmt.Sprintf(
		"When you click send, the %s server will be removed from your guild. You will not be able to retrieve savegames anymore.",
		serverName)
	return bot.confirm(cmd, title, confimationText)
}

// _configurationModal generates a modal for configuring a server based on the provided
// command and server specification.
func (bot Frontend) _configurationModal(cmd internal.BackendCmd, spec internal.ServerSpec) (events.APIGatewayProxyResponse, error) {
	paramsSpec := make(map[string]string, len(spec.Params))
	for env, label := range spec.Params {
		paramsSpec[env] = label
	}
	title := fmt.Sprintf("Configure %s server", spec.Name)
	return bot.modal(cmd, title, paramsSpec)
}

//===== Section: backend call

// Functions in this section call the backend to perform/finalize the
// requested command.

// callBackend queues up a BackendCmd for the backend.
func (bot Frontend) callBackend(cmd internal.BackendCmd) error {
	bot.Logger.Debug("calling backend command", zap.Any("cmd", cmd))
	return internal.QueueMarshalledCmd(bot.QueueUrl, cmd)
}

// genericConfirmedCall is used as the second step of a frontloop, after the first step
// replied with a confirmation modal.
//
// Note 1: This function is triggered AFTER user confirmation, so the function
// simply calls the backend.
//
// Note 2: Routing between step 1 and step 2 must be explicitly developed
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
// gathers the prompted spec and calls the backend.
func (bot Frontend) gameRegisterCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()
	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in gameRegisterCall", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
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
		bot.Logger.Error("error in gameRegisterCall", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

// engineTierRegisterCall is used as the step 2 of a frontloop, after the step 1
// replied with a configuration modal for the EngineTier. The function
// gathers the prompted spec and calls the backend.
func (bot Frontend) engineTierRegisterCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()
	cmd, err := internal.UnmarshallCustomID(msd.CustomID)
	if err != nil {
		bot.Logger.Error("error in engineTierRegisterCall", zap.String("culprit", "UnmarshallCustomIDAction"), zap.Error(err))
		bot.reply("🚫 Internal error")
	}
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token

	item := msd.Components[0]
	textInput := item.(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput)

	args := cmd.Args.(*internal.RegisterEngineTierArgs)
	args.Spec = textInput.Value
	cmd.Args = args

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in engineTierRegisterCall", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

// serverCreationCall is used as the step 2 of a frontloop, after the step 1
// replied with a server configuration modal. The function gathers the prompted
// parameters and calls the backend.
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

// serverStartCall sends a start command for the backend.
func (bot Frontend) serverStartCall(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()

	args := internal.StartArgs{
		ChannelID: itn.ChannelID,
	}

	// Get command arguments
	if len(acd.Options) > 0 {
		args.EngineTier = acd.Options[0].StringValue()
	}

	cmd := internal.BackendCmd{
		AppID: itn.AppID,
		Token: itn.Token,
		Args:  args,
	}

	if err := bot.callBackend(cmd); err != nil {
		bot.Logger.Error("error in serverStartCall", zap.String("culprit", "callBackend"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

// serverConfigurationCall is used as the step 2 of a frontloop, after the step 1
// replied with a server configuration modal. The function gathers the prompted
// parameters and calls the backend.
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
	targetID := acd.Options[0].Value.(string) // We shortcut to the value because discordgo API want to query user details

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
	targetID := acd.Options[0].Value.(string) // We shortcut to the value because discordgo API want to query user details

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

// stopServer stops the server for the given channel ID. It performs the
// following steps:
//  1. Retrieves the instance details from DynamoDB.
//  2. Verifies that the task is not already stopped.
//  3. If not, issues the stop request.
func (bot Frontend) stopServer(channelID string) (events.APIGatewayProxyResponse, error) {
	inst, err := internal.DynamodbScanFindFirst[internal.Instance](bot.InstanceTable, "ServerChannelID", channelID)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Check that the task is not already stopped
	// TODO: ensure that stopping an EC2 instance while it starts is safe.
	// If not, only allow stopping if the instance is running
	if inst.EngineID == "" {
		return bot.reply("🟥 Server offline")
	} else {
		instanceState, err := inst.GetState(bot.Lsdc2Stack.EcsClusterName)
		if err != nil {
			bot.Logger.Error("error in stopServer", zap.String("culprit", "GetState"), zap.Error(err))
			return bot.reply("🚫 Internal error")
		}
		if instanceState == internal.InstanceStateStopped {
			return bot.reply("🟥 Server offline")
		}
	}

	// Issue the task stop request
	bot.Logger.Debug("stopping: stop task", zap.Any("instance", inst))
	if err = inst.StopInstance(bot.BotEnv); err != nil {
		bot.Logger.Error("error in stopServer", zap.String("culprit", "StopTask"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("Server is going offline")
}

// serverStatus retrieves the status of the server associated with the
// given channel ID.
func (bot Frontend) serverStatus(channelID string) (events.APIGatewayProxyResponse, error) {
	inst, err := internal.DynamodbScanFindFirst[internal.Instance](bot.InstanceTable, "ServerChannelID", channelID)
	if err != nil {
		bot.Logger.Error("error in startServer", zap.String("culprit", "DynamodbScanFindFirst"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Status: offline
	if inst.EngineID == "" {
		return bot.reply("🟥 Server offline")
	}
	instanceState, err := inst.GetState(bot.Lsdc2Stack.EcsClusterName)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "GetState"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// Status: changing
	switch instanceState {
	case internal.InstanceStateStopped:
		return bot.reply("🟥 Server offline")
	case internal.InstanceStateStopping:
		return bot.reply("⚠️ Server is going offline")
	case internal.InstanceStateStarting:
		return bot.reply("⚠️ Server is starting. Please wait a few minutes")
	}

	// Status: online
	ip, err := inst.GetIP(bot.Lsdc2Stack.EcsClusterName)
	if err != nil {
		bot.Logger.Error("error in serverStatus", zap.String("culprit", "GetTaskIP"), zap.Error(err))
		return bot.reply(":thinking: Public IP not available, contact administrator")
	}

	return bot.reply("✅ Instance online ! ```%s```Open ports: %s", ip, inst.OpenPorts)
}

// savegameDownload creates a pre-signed URL for the savegame file stored in
// S3, for the given channel ID, and replies with the link.
func (bot Frontend) savegameDownload(channelID string) (events.APIGatewayProxyResponse, error) {
	// Retrieve server details
	srv := internal.Server{}
	err := internal.DynamodbGetItem(bot.ServerTable, channelID, &srv)
	if err != nil {
		bot.Logger.Error("error in savegameDownload", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if srv.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// Get the presigned URL
	url, err := internal.PresignGetS3Object(bot.Bucket, srv.Name, time.Minute)
	if err != nil {
		bot.Logger.Error("error in savegameDownload", zap.String("culprit", "PresignGetS3Object"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}

	// We don't use the bot.replyLink approach because S3 presigned URL are too long
	return bot.reply("Link to %s savegame: [Download](%s)", srv.Name, url)
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
	srv := internal.Server{}
	err := internal.DynamodbGetItem(bot.ServerTable, itn.ChannelID, &srv)
	if err != nil {
		bot.Logger.Error("error in savegameUpload", zap.String("culprit", "DynamodbGetItem"), zap.Error(err))
		return bot.reply("🚫 Internal error")
	}
	if srv.SpecName == "" {
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
	return bot.replyLink(url.String(), fmt.Sprintf("Open %s savegame upload page", srv.Name), "")
}

// Cache of the choices between lambda calls
// A bit hacky but it's a cheap way to avoid a table scan at each call
var __spinupScanCache []internal.ServerSpec
var __startScanCache []internal.EngineTier

// autocompleteSpinup returns an autocomplete response with the choices of
// registered games. Note that user inputs are completely ignored: they are not
// used to filter the choices.
func (bot Frontend) autocompleteSpinup(opt []*discordgo.ApplicationCommandInteractionDataOption) (events.APIGatewayProxyResponse, error) {
	var allSpec []internal.ServerSpec

	// Check for cache hit
	if __spinupScanCache != nil {
		allSpec = __spinupScanCache
	} else {
		var err error
		allSpec, err = internal.DynamodbScan[internal.ServerSpec](bot.ServerSpecTable)
		if err != nil {
			return internal.Error500(), fmt.Errorf("DynamodbScan / %w", err)
		}
		sort.Slice(allSpec, func(i, j int) bool {
			return allSpec[i].Name < allSpec[j].Name
		})
		__spinupScanCache = allSpec
	}

	partialValue := opt[0].StringValue()

	choices := make([]*discordgo.ApplicationCommandOptionChoice, len(allSpec))
	idx := 0
	for _, item := range allSpec {
		if partialValue == "" || strings.Contains(item.Name, partialValue) {
			choices[idx] = &discordgo.ApplicationCommandOptionChoice{
				Name:  item.Name, // This is the value displayed to the user
				Value: item.Name, // This is the value sent to the command
			}
			idx = idx + 1
		}
	}

	return bot.replyAutocomplete(choices[:idx])
}

// autocompleteStart returns an autocomplete response with the choices of
// registered engine tiers. Note that user inputs are completely ignored: they are not
// used to filter the choices.
func (bot Frontend) autocompleteStart(opt []*discordgo.ApplicationCommandInteractionDataOption) (events.APIGatewayProxyResponse, error) {
	var allTiers []internal.EngineTier

	// Check for cache hit
	if __startScanCache != nil {
		allTiers = __startScanCache
	} else {
		var err error
		allTiers, err = internal.DynamodbScan[internal.EngineTier](bot.EngineTierTable)
		if err != nil {
			return internal.Error500(), fmt.Errorf("DynamodbScan / %w", err)
		}
		sort.Slice(allTiers, func(i, j int) bool {
			return allTiers[i].Name < allTiers[j].Name
		})
		__startScanCache = allTiers
	}

	partialValue := opt[0].StringValue()

	choices := make([]*discordgo.ApplicationCommandOptionChoice, len(allTiers))
	idx := 0
	for _, item := range allTiers {
		if partialValue == "" || strings.Contains(item.Name, partialValue) {
			choices[idx] = &discordgo.ApplicationCommandOptionChoice{
				Name:  item.Name, // This is the value displayed to the user
				Value: item.Name, // This is the value sent to the command
			}
			idx = idx + 1
		}
	}

	return bot.replyAutocomplete(choices[:idx])
}

//===== Section: bot reply helpers

// ackMessage acknowledges to Discord that the ApplicationCommand is being handled
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

// ackComponent acknowledges to Discord that the MessageComponent is being handled
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
// specified message. It does not strictly look like a confirmation
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
// has a length limit (and thus fails on longer confirmation messages).
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
