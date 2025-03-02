package internal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

//===== Section: Constants

const (
	PermViewChannel        int64 = 0x0000000000000400
	PermReadHistory        int64 = 0x0000000000010000
	PermApplicationCommand int64 = 0x0000000080000000
	PermCreateInvite       int64 = 0x0000000000000001
	PermAdmin              int64 = 0x0000000000000008
)

func Everyone(guildID string) string {
	return guildID
}

func AllChannels(guildID string) string {
	allChan, _ := strconv.ParseInt(guildID, 10, 64)
	return fmt.Sprintf("%d", allChan-1)
}

func IsAdmin(member *discordgo.Member) bool {
	return (member.Permissions & PermAdmin) > 0
}

//===== Section: Guild commands

// The commands that are added to a guild uppon welcoming the bot
var __guildsCommands = []*discordgo.ApplicationCommand{
	{
		Name:        SpinupAPI,
		Description: "Start a new server instance",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:         "game-type",
				Description:  "Game type to start",
				Required:     true,
				Type:         discordgo.ApplicationCommandOptionString,
				Autocomplete: true,
			},
		},
	},
	{
		Name:        DestroyAPI,
		Description: "Destroy a server",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "server-name",
				Description: "The name of the server to destroy",
				Required:    true,
				Type:        discordgo.ApplicationCommandOptionString,
			},
		},
	},
	{
		Name:        InviteAPI,
		Description: "Invite a user to LSDC2 Role and/or server",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "member",
				Description: "The member invited",
				Required:    true,
				Type:        discordgo.ApplicationCommandOptionUser,
			},
		},
	},
	{
		Name:        KickAPI,
		Description: "Kick a user from an LSDC2 Role and/or server",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "member",
				Description: "The member removed",
				Required:    true,
				Type:        discordgo.ApplicationCommandOptionUser,
			},
		},
	},
	{
		Name:        StartAPI,
		Description: "Start a server instance (run in instance channel)",
	},
	{
		Name:        StopAPI,
		Description: "Stop a running server instance (run in instance channel)",
	},
	{
		Name:        StatusAPI,
		Description: "Give the status of a server instance (run in instance channel)",
	},
	{
		Name:        DownloadAPI,
		Description: "Retrieve the savegame of a server instance (run in instance channel)",
	},
	{
		Name:        UploadAPI,
		Description: "Upload a savegame to a server instance (run in instance channel)",
	},
}

func CreateGuildsCommands(sess *discordgo.Session, appID string, guildID string) ([]*discordgo.ApplicationCommand, error) {
	guildCmd := make([]*discordgo.ApplicationCommand, len(__guildsCommands))
	var err error
	for idx, cmd := range __guildsCommands {
		guildCmd[idx], err = sess.ApplicationCommandCreate(appID, guildID, cmd)
		if err != nil {
			return nil, err
		}
	}
	return guildCmd, nil
}

func DeleteGuildsCommands(sess *discordgo.Session, appID string, guildID string) error {
	registeredCmd, err := sess.ApplicationCommands(appID, guildID)
	if err != nil {
		return fmt.Errorf("discordgo.ApplicationCommands / %w", err)
	}
	for _, cmd := range registeredCmd {
		err := sess.ApplicationCommandDelete(appID, guildID, cmd.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

// CommandsWithNameInList filters a list of ApplicationCommand objects and
// returns only those whose names are present in the provided list of names.
//
// Parameters:
// - cmd: A slice of ApplicationCommand to be filtered.
// - names: A slice of strings containing the accepted command names.
func CommandsWithNameInList(cmd []*discordgo.ApplicationCommand, names []string) []*discordgo.ApplicationCommand {
	filteredCmd := []*discordgo.ApplicationCommand{}
	for _, cmd := range cmd {
		if slices.Contains(names, cmd.Name) {
			filteredCmd = append(filteredCmd, cmd)
		}
	}
	return filteredCmd
}

//===== Section: Guild commands permissions

// SetupAdminCommands disables the specified commands for all channel
// and users except for the LSDC2 admin channel and LSDC2 admins
func SetupAdminCommands(sess *discordgo.Session, appID string, guildID string, gc GuildConf, cmds []*discordgo.ApplicationCommand) error {
	for _, cmd := range cmds {
		perm := &discordgo.ApplicationCommandPermissionsList{
			Permissions: []*discordgo.ApplicationCommandPermissions{
				{
					ID:         Everyone(guildID),
					Type:       discordgo.ApplicationCommandPermissionTypeRole,
					Permission: false,
				},
				{
					ID:         AllChannels(guildID),
					Type:       discordgo.ApplicationCommandPermissionTypeChannel,
					Permission: false,
				},
				{
					ID:         gc.AdminRoleID,
					Type:       discordgo.ApplicationCommandPermissionTypeRole,
					Permission: true,
				},
				{
					ID:         gc.AdminChannelID,
					Type:       discordgo.ApplicationCommandPermissionTypeChannel,
					Permission: true,
				},
			},
		}
		if cmd.Name == InviteAPI {
			perm.Permissions = append(perm.Permissions,
				&discordgo.ApplicationCommandPermissions{
					ID:         gc.UserRoleID,
					Type:       discordgo.ApplicationCommandPermissionTypeRole,
					Permission: true,
				},
			)
		}
		err := sess.ApplicationCommandPermissionsEdit(appID, guildID, cmd.ID, perm)
		if err != nil {
			return err
		}
	}
	return nil
}

// SetupUserCommands disables the specified commands for all channel
// and users except for the LSDC2 admins and users. Note that the
// persmission is not sufficient for LSDC2 admins and user to run a
// command: a channel need to be further command enabled with a call
// to EnableChannelCommands
func SetupUserCommands(sess *discordgo.Session, appID string, guildID string, gc GuildConf, cmds []*discordgo.ApplicationCommand) error {
	for _, cmd := range cmds {
		perm := &discordgo.ApplicationCommandPermissionsList{
			Permissions: []*discordgo.ApplicationCommandPermissions{
				{
					ID:         Everyone(guildID),
					Type:       discordgo.ApplicationCommandPermissionTypeRole,
					Permission: false,
				},
				{
					ID:         AllChannels(guildID),
					Type:       discordgo.ApplicationCommandPermissionTypeChannel,
					Permission: false,
				},
				{
					ID:         gc.AdminRoleID,
					Type:       discordgo.ApplicationCommandPermissionTypeRole,
					Permission: true,
				},
				{
					ID:         gc.UserRoleID,
					Type:       discordgo.ApplicationCommandPermissionTypeRole,
					Permission: true,
				},
			},
		}
		err := sess.ApplicationCommandPermissionsEdit(appID, guildID, cmd.ID, perm)
		if err != nil {
			return err
		}
	}
	return nil
}

// EnableChannelCommands enable the specified commands on the specified channel.
func EnableChannelCommands(sess *discordgo.Session, appID string, guildID string, chanID string, cmds []*discordgo.ApplicationCommand) error {
	for _, cmd := range cmds {
		oldPerms, err := sess.ApplicationCommandPermissions(appID, guildID, cmd.ID)
		if err != nil {
			return err
		}
		newPerms := &discordgo.ApplicationCommandPermissionsList{
			Permissions: append(oldPerms.Permissions, &discordgo.ApplicationCommandPermissions{
				ID:         chanID,
				Type:       discordgo.ApplicationCommandPermissionTypeChannel,
				Permission: true,
			}),
		}
		err = sess.ApplicationCommandPermissionsEdit(appID, guildID, cmd.ID, newPerms)
		if err != nil {
			return err
		}
	}
	return nil
}

//===== Section: Channel commands permissions

// PrivateChannelOverwrite returns a permission to make a channel private (deny view)
func PrivateChannelOverwrite(guildID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:   Everyone(guildID),
		Type: discordgo.PermissionOverwriteTypeRole,
		Deny: PermViewChannel,
	}
}

// ViewHistoryInviteOverwrite returns a permission to view, view history
// and invites to the specified role
func ViewHistoryInviteOverwrite(roleID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:    roleID,
		Type:  discordgo.PermissionOverwriteTypeRole,
		Allow: PermViewChannel | PermReadHistory | PermCreateInvite,
	}
}

// ViewHistoryInviteOverwrite returns a permission to view, view history
// and run command to the specified role
func ViewHistoryAppcmdOverwrite(roleID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:    roleID,
		Type:  discordgo.PermissionOverwriteTypeRole,
		Allow: PermViewChannel | PermApplicationCommand | PermReadHistory,
	}
}

// HistoryAppcmdOverwrite returns a permission to view history and run
// command to the specified role
func HistoryAppcmdOverwrite(roleID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:    roleID,
		Type:  discordgo.PermissionOverwriteTypeRole,
		Allow: PermApplicationCommand | PermReadHistory,
	}
}

// AddUserView retrieves the permissions of a channel and append the view
// permission for the specified user
func AddUserView(sess *discordgo.Session, channelID string, userID string) error {
	channel, err := sess.Channel(channelID)
	if err != nil {
		return err
	}
	newPerms := append(
		channel.PermissionOverwrites, &discordgo.PermissionOverwrite{
			ID:    userID,
			Type:  discordgo.PermissionOverwriteTypeMember,
			Allow: PermViewChannel,
		},
	)
	_, err = sess.ChannelEdit(channelID, &discordgo.ChannelEdit{
		PermissionOverwrites: newPerms,
		Position:             &channel.Position,
	})

	return err
}

// RemoveUserView retrieves the permissions of a channel and remove all
// user defined permission for the specified user
func RemoveUserView(sess *discordgo.Session, channelID string, userID string) error {
	channel, err := sess.Channel(channelID)
	if err != nil {
		return err
	}
	userFound := false
	var userPermIdx int
	for idx, perm := range channel.PermissionOverwrites {
		if perm.ID == userID {
			userFound = true
			userPermIdx = idx
		}
	}

	if userFound {
		newPerms := channel.PermissionOverwrites
		newPerms[userPermIdx] = newPerms[len(newPerms)-1]
		newPerms = newPerms[:len(newPerms)-1]
		_, err = sess.ChannelEdit(channelID, &discordgo.ChannelEdit{
			PermissionOverwrites: newPerms,
			Position:             &channel.Position,
		})
	}

	return err
}

//===== Section: OAuth2

// BearerSession returns a discordgo session with a Bearer token using Client Credentials OAuth2 grant
func BearerSession(clientID string, clientSecret string, scope string) (sess *discordgo.Session, cleanup func(), err error) {
	token, err := GetBearerToken(clientID, clientSecret, scope)
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() {
		err := RevokeBearerToken(clientID, clientSecret, token)
		if err != nil {
			err = fmt.Errorf("RevokeBearerToken / %w", err)
			panic(err)
		}
	}
	sess, err = discordgo.New("Bearer " + token)
	if err != nil {
		cleanup()
	}

	return
}

// GetBearerToken returns a Discord Bearer token using Client Credentials OAuth2 grant
func GetBearerToken(clientID string, clientSecret string, scope string) (string, error) {
	tokenUrl := "https://discord.com/api/oauth2/token"

	data := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {scope},
	}
	req, err := http.NewRequest("POST", tokenUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	out := struct {
		AccessToken string `json:"access_token"`
	}{}
	if err = json.Unmarshal(bodyBytes, &out); err != nil {
		return "", err
	}

	return out.AccessToken, nil
}

// RevokeBearerToken revokes the specified Bearer token using Client Credentials OAuth2 grant
func RevokeBearerToken(clientID string, clientSecret string, token string) error {
	tokenUrl := "https://discord.com/api/oauth2/token/revoke"

	data := url.Values{
		"token": {token},
	}
	req, err := http.NewRequest("POST", tokenUrl, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("request code %d", resp.StatusCode)
	}

	return nil
}
