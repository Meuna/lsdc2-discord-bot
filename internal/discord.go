package internal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

//
// Contants
//

const (
	PermViewChannel        int64 = 0x0000000000000400
	PermReadHistory        int64 = 0x0000000000010000
	PermApplicationCommand int64 = 0x0000000080000000
	PermCreateInvite       int64 = 0x0000000000000001
)

func EveryoneRole(guildID string) string {
	return guildID
}

func AllChannel(guildID string) string {
	allChan, _ := strconv.ParseInt(guildID, 10, 64)
	return fmt.Sprintf("%d", allChan-1)
}

//
// Channel role permissions overwrite
//

func PrivateChannelOverwrite(guildID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:   EveryoneRole(guildID),
		Type: discordgo.PermissionOverwriteTypeRole,
		Deny: PermViewChannel,
	}
}

func ViewInviteOverwrite(roleID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:    roleID,
		Type:  discordgo.PermissionOverwriteTypeRole,
		Allow: PermViewChannel | PermReadHistory | PermCreateInvite,
	}
}

func ViewAppcmdOverwrite(roleID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:    roleID,
		Type:  discordgo.PermissionOverwriteTypeRole,
		Allow: PermViewChannel | PermApplicationCommand | PermReadHistory,
	}
}

func AppcmdOverwrite(roleID string) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{
		ID:    roleID,
		Type:  discordgo.PermissionOverwriteTypeRole,
		Allow: PermApplicationCommand | PermReadHistory,
	}
}

//
// Bearer helpers
//

func BearerSession(clientID string, clientSecret string, scope string) (sess *discordgo.Session, cleanup func(), err error) {
	token, err := GetBearerToken(clientID, clientSecret, scope)
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() {
		err := RevokeBearerToken(clientID, clientSecret, token)
		if err != nil {
			err = fmt.Errorf("RevokeBearerToken failed %s", err)
			panic(err)
		}
	}
	sess, err = discordgo.New("Bearer " + token)
	if err != nil {
		cleanup()
	}

	return
}

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

//
// Commands permission helpers
//

func DisableCommands(sess *discordgo.Session, appID string, guildID string, cmds []*discordgo.ApplicationCommand) error {
	for _, cmd := range cmds {
		perm := &discordgo.ApplicationCommandPermissionsList{
			Permissions: []*discordgo.ApplicationCommandPermissions{
				{
					ID:         EveryoneRole(guildID),
					Type:       1,
					Permission: false,
				},
				{
					ID:         AllChannel(guildID),
					Type:       3,
					Permission: false,
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

func SetupAdminCommands(sess *discordgo.Session, appID string, guildID string, gc GuildConf, cmds []*discordgo.ApplicationCommand) error {
	for _, cmd := range cmds {
		perm := &discordgo.ApplicationCommandPermissionsList{
			Permissions: []*discordgo.ApplicationCommandPermissions{
				{
					ID:         EveryoneRole(guildID),
					Type:       1,
					Permission: false,
				},
				{
					ID:         AllChannel(guildID),
					Type:       3,
					Permission: false,
				},
				{
					ID:         gc.AdminRoleID,
					Type:       1,
					Permission: true,
				},
				{
					ID:         gc.AdminChannelID,
					Type:       3,
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

func SetupUserCommands(sess *discordgo.Session, appID string, guildID string, gc GuildConf, cmds []*discordgo.ApplicationCommand) error {
	for _, cmd := range cmds {
		perm := &discordgo.ApplicationCommandPermissionsList{
			Permissions: []*discordgo.ApplicationCommandPermissions{
				{
					ID:         EveryoneRole(guildID),
					Type:       1,
					Permission: false,
				},
				{
					ID:         AllChannel(guildID),
					Type:       3,
					Permission: false,
				},
				{
					ID:         gc.AdminRoleID,
					Type:       1,
					Permission: true,
				},
				{
					ID:         gc.UserRoleID,
					Type:       1,
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

func EnableChannelCommands(sess *discordgo.Session, appID string, guildID string, chanID string, cmds []*discordgo.ApplicationCommand) error {
	for _, cmd := range cmds {
		oldPerm, err := sess.ApplicationCommandPermissions(appID, guildID, cmd.ID)
		if err != nil {
			return err
		}
		newPerm := &discordgo.ApplicationCommandPermissionsList{
			Permissions: append(oldPerm.Permissions, &discordgo.ApplicationCommandPermissions{
				ID:         chanID,
				Type:       3,
				Permission: true,
			}),
		}
		err = sess.ApplicationCommandPermissionsEdit(appID, guildID, cmd.ID, newPerm)
		if err != nil {
			return err
		}
	}
	return nil
}

//
// Commands helpers
//

var lsdc2Commands = []*discordgo.ApplicationCommand{
	{
		Name:        DestroyAPI,
		Description: "Destroy a server",
		// DefaultMemberPermissions: &discordgo.PermissionManageServer,
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

func SetupLsdc2Commands(sess *discordgo.Session, appID string, guildID string) error {
	for _, cmd := range lsdc2Commands {
		fmt.Printf("Bootstraping %s: %s command\n", guildID, cmd.Name)
		_, err := sess.ApplicationCommandCreate(appID, guildID, cmd)
		if err != nil {
			return err
		}
	}
	return nil
}

func GetAllCommands(sess *discordgo.Session, appID string, guildID string) ([]*discordgo.ApplicationCommand, error) {
	globalCmd, err := sess.ApplicationCommands(appID, "")
	if err != nil {
		return nil, err
	}
	guildCmd, err := sess.ApplicationCommands(appID, guildID)
	if err != nil {
		return nil, err
	}
	return append(globalCmd, guildCmd...), nil
}

func FilterCommandsByName(cmd []*discordgo.ApplicationCommand, names []string) []*discordgo.ApplicationCommand {
	filteredCmd := []*discordgo.ApplicationCommand{}
	for _, cmd := range cmd {
		if Contains(names, cmd.Name) {
			filteredCmd = append(filteredCmd, cmd)
		}
	}
	return filteredCmd
}
