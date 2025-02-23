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
		return bot.error404(), nil
	}
}

func InitFrondend() Frontend {
	bot, err := internal.ParseEnv()
	if err != nil {
		panic(err)
	}
	return Frontend{bot}
}

type Frontend struct {
	internal.BotEnv
}

func (bot Frontend) json200(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       msg,
		Headers: map[string]string{
			"content-type": "application/json",
		},
	}
}

func (bot Frontend) html200(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       msg,
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
}

func (bot Frontend) error401(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 401,
		Body:       msg,
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
}

func (bot Frontend) error404() events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 404,
		Body:       "404: content not found",
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
}

func (bot Frontend) error500() events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 500,
		Body:       "500: content not found",
		Headers: map[string]string{
			"content-type": "text/html",
		},
	}
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
		return bot.error500(), fmt.Errorf("_parseQuery failed: %s", err)
	}

	// Verify MAC and TTL
	if !internal.VerifyMacWithTTL(key, []byte(channelID), eol, mac) {
		return bot.error401("401: MAC verification failed"), nil
	}
	if time.Now().Unix() > eol {
		return bot.error401("401: MAC expired"), nil
	}

	// Retrieve instance
	inst := internal.ServerInstance{}
	err = internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		return bot.error500(), fmt.Errorf("DynamodbGetItem failed: %s", err)
	}
	if inst.SpecName == "" {
		return bot.error500(), fmt.Errorf("instance %s not found", channelID)
	}

	// Presign S3 PUT
	url, err := internal.PresignPutS3Object(bot.SaveGameBucket, inst.Name, time.Minute)
	if err != nil {
		return bot.error500(), fmt.Errorf("PresignGetS3Object failed: %s", err)
	}

	r := strings.NewReplacer("{{serverName}}", serverName, "{{presignedUrl}}", url)
	uploadPageWithPutUrl := r.Replace(uploadPage)

	return bot.html200(uploadPageWithPutUrl), nil
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
		return bot.error401(""), errors.New("signature check failed")
	}

	var itn discordgo.Interaction
	if err := itn.UnmarshalJSON([]byte(request.Body)); err != nil {
		return bot.error500(), fmt.Errorf("UnmarshalJSON failed: %s", err)
	}

	switch itn.Type {
	case discordgo.InteractionPing:
		fmt.Println("Received PING interaction")
		return bot.json200(`{"type": 1}`), nil

	case discordgo.InteractionApplicationCommand:
		fmt.Println("Received application command interaction")
		return bot.routeCommand(itn)

	case discordgo.InteractionMessageComponent:
		fmt.Println("Received message component interaction")
		return bot.routeMessageComponent(itn)

	case discordgo.InteractionApplicationCommandAutocomplete:
		fmt.Println("Received autocomplete interaction")
		return bot.routeAutocomplete(itn)

	case discordgo.InteractionModalSubmit:
		fmt.Println("Received modal interaction")
		return bot.routeModalSubmit(itn)

	default:
		return bot.error500(), fmt.Errorf("unknown interaction type %v", itn.Type)
	}
}

func (bot Frontend) checkDiscordSignature(request events.LambdaFunctionURLRequest) bool {
	pkey, _ := hex.DecodeString(bot.Pkey)
	sig, _ := hex.DecodeString(request.Headers["x-signature-ed25519"])
	pl := []byte(request.Headers["x-signature-timestamp"] + request.Body)

	return ed25519.Verify(pkey, pl, sig)
}

func (bot Frontend) callBackend(cmd internal.BackendCmd) error {
	fmt.Printf("Calling '%s' backend function with arguments %+v\n", cmd.Action(), cmd.Args)
	return internal.QueueMarshalledAction(bot.QueueUrl, cmd)
}

//
//	Bot reply
//

func (bot Frontend) ackMessage() (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return bot.error500(), fmt.Errorf("marshal failed: %s", err)
	}
	return bot.json200(string(jsonBytes[:])), nil
}

func (bot Frontend) ackComponent() (events.APIGatewayProxyResponse, error) {
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return bot.error500(), fmt.Errorf("marshal failed: %s", err)
	}
	return bot.json200(string(jsonBytes[:])), nil
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
		return bot.error500(), fmt.Errorf("marshal failed: %s", err)
	}
	return bot.json200(string(jsonBytes[:])), nil
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
		return bot.error500(), fmt.Errorf("marshal failed: %s", err)
	}
	return bot.json200(string(jsonBytes[:])), nil
}

func (bot Frontend) confirm(itnSrc discordgo.Interaction, cmd internal.BackendCmd, msg string, fmtarg ...interface{}) (events.APIGatewayProxyResponse, error) {
	customID, err := internal.MarshalCustomIDAction(cmd)
	if err != nil {
		fmt.Printf("MarshalCustomIDAction failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}

	_componentMessageOrigin = &itnSrc
	itnResp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf(msg, fmtarg...),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.Button{
							Label:    "Yes",
							Style:    discordgo.PrimaryButton,
							CustomID: customID,
						},
						discordgo.Button{
							Label:    "Cancel",
							Style:    discordgo.SecondaryButton,
							CustomID: "cancel",
						},
					},
				},
			},
		},
	}
	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return bot.error500(), fmt.Errorf("marshal failed: %s", err)
	}
	return bot.json200(string(jsonBytes[:])), nil
}

func (bot Frontend) modal(cmd internal.BackendCmd, title string, paramSpec map[string]string) (events.APIGatewayProxyResponse, error) {
	customID, err := internal.MarshalCustomIDAction(cmd)
	if err != nil {
		fmt.Printf("MarshalCustomIDAction failed: %s\n", err)
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
		return bot.error500(), fmt.Errorf("marshal failed: %s", err)
	}
	return bot.json200(string(jsonBytes[:])), nil
}

func (bot Frontend) textPrompt(cmd internal.BackendCmd, title string, label string) (events.APIGatewayProxyResponse, error) {
	customID, err := internal.MarshalCustomIDAction(cmd)
	if err != nil {
		fmt.Printf("MarshalCustomIDAction failed: %s\n", err)
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
							Label:    label,
							Style:    discordgo.TextInputParagraph,
							CustomID: "text",
							Required: true,
						},
					},
				},
			},
		},
	}

	jsonBytes, err := json.Marshal(itnResp)
	if err != nil {
		return bot.error500(), fmt.Errorf("marshal failed: %s", err)
	}
	return bot.json200(string(jsonBytes[:])), nil
}

//
//	Command routing
//

func (bot Frontend) routeCommand(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	fmt.Printf("Routing '%s' application command\n", acd.Name)

	switch acd.Name {
	case internal.RegisterGameAPI:
		return bot.requestNewGameRegister(itn)
	case internal.BootstrapAPI:
		return bot.confirmGuildBootstrap(itn)
	case internal.SpinupAPI:
		return bot.configureServerCreation(itn)
	case internal.DestroyAPI:
		return bot.confirmServerDestruction(itn)
	case internal.InviteAPI:
		return bot.requestMemberInvite(itn)
	case internal.KickAPI:
		return bot.requestMemberKick(itn)
	case internal.StartAPI:
		return bot.startServer(itn.ChannelID)
	case internal.StopAPI:
		return bot.stopServer(itn.ChannelID)
	case internal.StatusAPI:
		return bot.serverStatus(itn.ChannelID)
	case internal.DownloadAPI:
		return bot.savegameDownload(itn.ChannelID)
	case internal.UploadAPI:
		return bot.savegameUpload(itn.ChannelID)
	default:
		fmt.Printf("Unknown command %s\n", acd.Name)
		return bot.reply("I don't understand ¯\\_(ツ)_/¯")
	}
}

// Very unreliable state persistance between lambda calls
// If it fail, users can still manually delete the origin message
var _componentMessageOrigin *discordgo.Interaction

func (bot Frontend) routeMessageComponent(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	// Delete the origin message if it exists
	if _componentMessageOrigin != nil {
		sess, err := discordgo.New("Bot " + bot.Token)
		if err != nil {
			fmt.Printf("discordgo.New failed: %s\n", err)
		}
		err = sess.InteractionResponseDelete(_componentMessageOrigin)
		if err != nil {
			fmt.Printf("InteractionResponseDelete failed: %s\n", err)
		}
		_componentMessageOrigin = nil
	}

	mcd := itn.MessageComponentData()

	// Do nothing if canceled
	if mcd.CustomID == "cancel" {
		fmt.Printf("Action canceled")
		return bot.ackComponent()
	}

	// Or proceed with action
	cmd, err := internal.UnmarshallCustomIDAction(mcd.CustomID)
	if err != nil {
		fmt.Printf("UnmarshallCustomIDAction failed: %s\n", err)
		bot.reply("🚫 Internal error")
	}
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token

	// Stop the server if needed
	if cmd.Action() == internal.DestroyAPI {
		args := cmd.Args.(*internal.DestroyArgs)
		bot.stopServer(args.ChannelID)
	}

	// And finally call the backend
	if err := bot.callBackend(cmd); err != nil {
		fmt.Printf("callBackend failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}

	return bot.ackMessage()
}

func (bot Frontend) routeModalSubmit(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()

	cmd, err := internal.UnmarshallCustomIDAction(msd.CustomID)
	if err != nil {
		fmt.Printf("UnmarshallCustomIDAction failed: %s\n", err)
		bot.reply("🚫 Internal error")
	}

	switch cmd.Action() {
	case internal.RegisterGameAPI:
		return bot.requestNewGameRegister(itn)
	case internal.SpinupAPI:
		return bot.requestServerCreation(itn)
	default:
		fmt.Printf("Unknown command %s\n", cmd.Action())
		return bot.reply("I don't understand ¯\\_(ツ)_/¯")
	}
}

func (bot Frontend) routeAutocomplete(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	fmt.Printf("DATA: %+v\n", itn)
	return bot.reply("🚫 Not implemented yet")
}

//
//	Backend commands
//

func (bot Frontend) requestNewGameRegister(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	args := internal.RegisterGameArgs{}

	switch itn.Type {
	case discordgo.InteractionApplicationCommand:
		acd := itn.ApplicationCommandData()
		for _, opt := range acd.Options {
			if opt.Name == internal.RegisterGameAPISpecUrlOpt {
				args.SpecUrl = opt.StringValue()
			} else if opt.Name == internal.RegisterGameAPIOverwriteOpt {
				args.Overwrite = opt.BoolValue()
			} else {
				fmt.Printf("Unknown option %s", opt.Name)
				return bot.reply("🚫 Internal error")
			}
		}
		if args.SpecUrl == "" {
			cmd := internal.BackendCmd{Args: &args}
			return bot.textPrompt(cmd, "Register new game", "Game LSDC2 spec")
		}

	case discordgo.InteractionModalSubmit:
		msd := itn.ModalSubmitData()
		cmdModal, err := internal.UnmarshallCustomIDAction(msd.CustomID)
		if err != nil {
			fmt.Printf("UnmarshallCustomIDAction failed: %s\n", err)
			bot.reply("🚫 Internal error")
		}

		item := msd.Components[0]
		textInput := item.(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput)
		args.Spec = textInput.Value
		args.Overwrite = cmdModal.Args.(*internal.RegisterGameArgs).Overwrite
	}
	cmd := internal.BackendCmd{}
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token
	cmd.Args = &args

	if err := bot.callBackend(cmd); err != nil {
		fmt.Printf("callBackend failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

func (bot Frontend) confirmGuildBootstrap(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	cmd := internal.BackendCmd{
		Args: &internal.BootstrapArgs{
			GuildID: itn.GuildID,
		},
	}
	return bot.confirm(itn, cmd, "Confirm LSDC2 bootstrap for your guild ?")
}

func (bot Frontend) confirmServerDestruction(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	serverName := acd.Options[0].StringValue()

	// Retrieve the chanel ID
	inst := internal.ServerInstance{}
	err := internal.DynamodbScanFind(bot.InstanceTable, "name", serverName, &inst)
	if err != nil {
		fmt.Printf("DynamodbScanFind failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	if inst.ChannelID == "" {
		return bot.reply("🚫 Server %s not found", serverName)
	}

	cmd := internal.BackendCmd{
		Args: &internal.DestroyArgs{
			ChannelID: inst.ChannelID,
		},
	}
	return bot.confirm(itn, cmd, "Confirm destruction of %s ?", serverName)
}

func (bot Frontend) configureServerCreation(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	acd := itn.ApplicationCommandData()
	gameName := acd.Options[0].StringValue()

	spec := internal.ServerSpec{}
	err := internal.DynamodbGetItem(bot.SpecTable, gameName, &spec)
	if err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	if spec.Name == "" {
		return bot.reply("⚠ Game spec %s not found (this should not happen)", gameName)
	}

	cmd := internal.BackendCmd{
		Args: &internal.SpinupArgs{
			GameName: gameName,
			GuildID:  itn.GuildID,
		},
	}

	if len(spec.EnvParamMap) > 0 {
		paramSpec := make(map[string]string, len(spec.EnvParamMap))
		for env, label := range spec.EnvParamMap {
			paramSpec[env] = label
		}
		title := fmt.Sprintf("Configure %s server", gameName)
		return bot.modal(cmd, title, paramSpec)
	} else {
		return bot.confirm(itn, cmd, "Confirm %s server spinup ?", gameName)
	}
}

func (bot Frontend) requestServerCreation(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
	msd := itn.ModalSubmitData()

	cmd, err := internal.UnmarshallCustomIDAction(msd.CustomID)
	if err != nil {
		fmt.Printf("UnmarshallCustomIDAction failed: %s\n", err)
		bot.reply("🚫 Internal error")
	}

	args := cmd.Args.(*internal.SpinupArgs)
	args.Env = make(map[string]string, len(msd.Components))
	for _, item := range msd.Components {
		textInput := item.(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput)
		key := textInput.CustomID
		value := textInput.Value
		args.Env[key] = value
	}
	cmd.Args = args
	cmd.AppID = itn.AppID
	cmd.Token = itn.Token

	if err := bot.callBackend(cmd); err != nil {
		fmt.Printf("callBackend failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

func (bot Frontend) requestMemberInvite(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
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
		fmt.Printf("callBackend failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	return bot.ackMessage()
}

func (bot Frontend) requestMemberKick(itn discordgo.Interaction) (events.APIGatewayProxyResponse, error) {
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
		fmt.Printf("callBackend failed: %s\n", err)
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
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// Check that the task is not yet running
	if inst.TaskArn != "" {
		task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
		if err != nil {
			fmt.Printf("DescribeTask failed: %s\n", err)
			return bot.reply("🚫 Internal error")
		}
		if task != nil {
			switch internal.GetTaskStatus(task) {
			case internal.TaskStopping:
				return bot.reply("⚠ Server is going offline. Please wait and try again")
			case internal.TaskProvisioning:
				return bot.reply("⚠ Server is starting. Please wait a few minutes")
			case internal.TaskContainerStopping:
				return bot.reply("⚠ Container is going offline. Please wait and try again")
			case internal.TaskContainerProvisioning:
				return bot.reply("⚠ Container is starting. Please wait a few minutes")
			case internal.TaskRunning:
				return bot.serverStatus(channelID)
			}
			// No match == we can start a server
		}
	}

	taskArn, err := internal.StartTask(inst, bot.Lsdc2Stack)
	if err != nil {
		fmt.Printf("StartTask failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	inst.TaskArn = taskArn
	err = internal.DynamodbPutItem(bot.InstanceTable, inst)
	if err != nil {
		fmt.Printf("DynamodbPutItem failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("✅ Server starting (wait few minutes)")
}

func (bot Frontend) stopServer(channelID string) (events.APIGatewayProxyResponse, error) {
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	// Check that the task is not yet running
	if inst.TaskArn == "" {
		return bot.reply("🟥 Server offline")
	}

	if err = internal.StopTask(inst, bot.Lsdc2Stack); err != nil {
		fmt.Printf("StopTask failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("⚠ Server is going offline")
}

func (bot Frontend) serverStatus(channelID string) (events.APIGatewayProxyResponse, error) {
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	spec := internal.ServerSpec{}
	err = internal.DynamodbGetItem(bot.SpecTable, inst.SpecName, &spec)
	if err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}

	if inst.TaskArn == "" {
		return bot.reply("🟥 Server offline")
	}
	task, err := internal.DescribeTask(inst, bot.Lsdc2Stack)
	if err != nil {
		fmt.Printf("DescribeTask failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}

	switch internal.GetTaskStatus(task) {
	case internal.TaskStopped:
		return bot.reply("🟥 Server offline")
	case internal.TaskStopping:
		return bot.reply("⚠ Server is going offline")
	case internal.TaskProvisioning:
		return bot.reply("⚠ Server is starting. Please wait a few minutes")
	case internal.TaskContainerStopping:
		return bot.reply("⚠ Server is going offline")
	case internal.TaskContainerProvisioning:
		return bot.reply("⚠ Container is starting. We're close !")
	}

	ip, err := internal.GetTaskIP(task, bot.Lsdc2Stack)
	if err != nil {
		fmt.Printf("GetTaskIP failed with error: %s\n", err)
		return bot.reply("⚠ Public IP not available, contact administrator")
	}
	return bot.reply("✅ Server online at %s (open ports: %s)", ip, spec.OpenPorts())
}

func (bot Frontend) savegameDownload(channelID string) (events.APIGatewayProxyResponse, error) {
	// Check that we are in a server channel
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	if inst.SpecName == "" {
		return bot.reply("🚫 Internal error. Are you in a server channel ?")
	}

	url, err := internal.PresignGetS3Object(bot.SaveGameBucket, inst.Name, time.Minute)
	if err != nil {
		fmt.Printf("PresignGetS3Object failed: %s\n", err)
		return bot.reply("🚫 Internal error")
	}
	return bot.reply("Link to %s savegame: [Download](%s)", inst.Name, url)
}

func (bot Frontend) savegameUpload(channelID string) (events.APIGatewayProxyResponse, error) {
	// Check that we are in a server channel
	inst := internal.ServerInstance{}
	err := internal.DynamodbGetItem(bot.InstanceTable, channelID, &inst)
	if err != nil {
		fmt.Printf("DynamodbGetItem failed: %s\n", err)
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
		Host:     "krbcuodbyr7qljkz6y2dsiun2q0igotx.lambda-url.eu-west-3.on.aws",
		Path:     "upload",
		RawQuery: values.Encode(),
	}
	return bot.replyLink(url.String(), "Open upload page", "%s savegame", inst.Name)
}
