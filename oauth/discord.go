package oauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

func init() {
	Register("discord", &DiscordProvider{})
}

// DiscordProvider implements OAuth for Discord
type DiscordProvider struct{}

type discordOAuthResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

type discordUser struct {
	UID  string `json:"id"`
	ID   string `json:"username"`
	Name string `json:"global_name"`
}

// DiscordAuthorizationURL keeps scopes and redirect URI under server control.
func DiscordAuthorizationURL(state, intent string) (string, error) {
	values := url.Values{
		"client_id":     {system_setting.GetDiscordSettings().ClientId},
		"redirect_uri":  {fmt.Sprintf("%s/oauth/discord", system_setting.ServerAddress)},
		"response_type": {"code"},
		"state":         {state},
	}
	scopes := []string{"identify"}
	if intent == model.AuthFlowIntentLogin {
		snapshot, err := system_setting.GetDiscordGuildRulesSnapshot()
		if err != nil {
			return "", NewOAuthError(common.NewMessage("Unable to verify Discord server access. Please start sign-in again"))
		}
		if len(snapshot.Rules) > 0 {
			scopes = append(scopes, "guilds")
			values.Set("prompt", "consent")
			for _, rule := range snapshot.Rules {
				if slices.ContainsFunc(rule.RoleIDs, func(role string) bool { return role != rule.GuildID }) {
					scopes = append(scopes, "guilds.members.read")
					break
				}
			}
		}
	}
	values.Set("scope", strings.Join(scopes, " "))
	return "https://discord.com/oauth2/authorize?" + values.Encode(), nil
}

// CheckLoginAdmission is deliberately separate from identity lookup: binding
// and step-up verification do not apply the login-only membership policy.
func (p *DiscordProvider) CheckLoginAdmission(ctx context.Context, token *OAuthToken) (result error) {
	retry := NewOAuthError(common.NewMessage("Unable to verify Discord server access. Please start sign-in again"))
	snapshot, err := system_setting.GetDiscordGuildRulesSnapshot()
	if err != nil {
		return retry
	}
	defer func() {
		// A revision also catches a change followed by restoration of the old rules.
		if !system_setting.DiscordGuildRulesVersionCurrent(snapshot.Version) {
			result = retry
		}
	}()
	if len(snapshot.Rules) == 0 {
		return nil
	}
	if token == nil || token.AccessToken == "" || !slices.Contains(strings.Fields(token.Scope), "guilds") {
		return retry
	}
	// Each request retains the existing five-second Discord timeout. The entire
	// decision gets at most three such windows, regardless of the rule count.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var guilds []struct {
		ID string `json:"id"`
	}
	status, err := discordMembershipRequest(ctx, token.AccessToken, "/users/@me/guilds?limit=200", &guilds)
	if err != nil || status != http.StatusOK || guilds == nil {
		return retry
	}
	membership := make(map[string]bool, len(guilds))
	for _, guild := range guilds {
		id, err := system_setting.NormalizeDiscordSnowflake(guild.ID)
		if err != nil || id != guild.ID {
			return retry
		}
		membership[id] = true
	}
	// Resolve membership-only rules first; unrelated member scopes/errors must
	// not overturn a rule that is already satisfied. @everyone is implicit.
	for _, rule := range snapshot.Rules {
		if membership[rule.GuildID] && (len(rule.RoleIDs) == 0 || (len(rule.RoleIDs) == 1 && rule.RoleIDs[0] == rule.GuildID)) {
			return nil
		}
	}
	unknown := false
	for _, rule := range snapshot.Rules {
		if !membership[rule.GuildID] {
			continue
		}
		if !slices.Contains(strings.Fields(token.Scope), "guilds.members.read") {
			return retry
		}
		var member struct {
			Roles []string `json:"roles"`
		}
		status, err := discordMembershipRequest(ctx, token.AccessToken, "/users/@me/guilds/"+rule.GuildID+"/member", &member)
		// Do not retry rate-limited buckets or keep querying with an invalid token.
		// Ending this attempt respects any Retry-After without unbounded waiting.
		if ctx.Err() != nil || status == http.StatusUnauthorized || status == http.StatusTooManyRequests {
			return retry
		}
		if status == http.StatusNotFound {
			continue
		}
		if err != nil || status != http.StatusOK || member.Roles == nil {
			unknown = true
			continue
		}
		valid := true
		for _, role := range member.Roles {
			id, err := system_setting.NormalizeDiscordSnowflake(role)
			if err != nil || id != role {
				valid = false
				break
			}
		}
		if !valid {
			unknown = true
			continue
		}
		matched := true
		for _, required := range rule.RoleIDs {
			if required != rule.GuildID && !slices.Contains(member.Roles, required) {
				matched = false
				break
			}
		}
		if matched {
			return nil
		}
	}
	if unknown {
		return retry
	}
	return &AccessDeniedError{Message: "Your Discord account does not meet the server or role requirements"}
}

// discordMembershipRequest reads only a bounded, complete JSON response from a
// fixed Discord endpoint. Neither response bodies nor credentials escape it.
func discordMembershipRequest(ctx context.Context, token, path string, result any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://discord.com/api/v10"+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil
	}
	const maxResponseBytes = 2 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return response.StatusCode, err
	}
	if len(body) > maxResponseBytes {
		return response.StatusCode, fmt.Errorf("Discord membership response exceeds %d bytes", maxResponseBytes)
	}
	return response.StatusCode, common.Unmarshal(body, result)
}

func (p *DiscordProvider) GetName() string {
	return "Discord"
}

func (p *DiscordProvider) IsEnabled() bool {
	return system_setting.GetDiscordSettings().Enabled
}

func (p *DiscordProvider) ExchangeToken(ctx context.Context, code string, c *gin.Context) (*OAuthToken, error) {
	if code == "" {
		return nil, NewOAuthError(common.NewMessage(msgInvalidCode))
	}

	settings := system_setting.GetDiscordSettings()
	redirectUri := fmt.Sprintf("%s/oauth/discord", system_setting.ServerAddress)
	values := url.Values{}
	values.Set("client_id", settings.ClientId)
	values.Set("client_secret", settings.ClientSecret)
	values.Set("code", code)
	values.Set("grant_type", "authorization_code")
	values.Set("redirect_uri", redirectUri)

	logger.LogDebug(ctx, "[OAuth-Discord] ExchangeToken: redirect_uri=%s", redirectUri)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://discord.com/api/v10/oauth2/token", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := http.Client{
		Timeout: 5 * time.Second,
	}
	res, err := client.Do(req)
	if err != nil {
		logger.LogError(ctx, common.LogText("[OAuth-Discord] ExchangeToken error: %s", err.Error()))
		return nil, NewOAuthErrorWithRaw(common.NewMessage(msgConnectFailed, map[string]any{"provider": "Discord"}), err.Error())
	}
	defer res.Body.Close()

	logger.LogDebug(ctx, "[OAuth-Discord] ExchangeToken response status: %d", res.StatusCode)

	var discordResponse discordOAuthResponse
	if res.StatusCode != http.StatusOK {
		return nil, NewOAuthError(common.NewMessage(msgTokenFailed, map[string]any{"provider": "Discord"}))
	}
	err = common.DecodeJson(res.Body, &discordResponse)
	if err != nil {
		logger.LogError(ctx, common.LogText("[OAuth-Discord] ExchangeToken decode error: %s", err.Error()))
		return nil, err
	}

	if discordResponse.AccessToken == "" {
		logger.LogError(ctx, common.LogText("[OAuth-Discord] ExchangeToken failed: empty access token"))
		return nil, NewOAuthError(common.NewMessage(msgTokenFailed, map[string]any{"provider": "Discord"}))
	}

	logger.LogDebug(ctx, "[OAuth-Discord] ExchangeToken success: scope=%s", discordResponse.Scope)

	return &OAuthToken{
		AccessToken:  discordResponse.AccessToken,
		TokenType:    discordResponse.TokenType,
		RefreshToken: discordResponse.RefreshToken,
		ExpiresIn:    discordResponse.ExpiresIn,
		Scope:        discordResponse.Scope,
		IDToken:      discordResponse.IDToken,
	}, nil
}

func (p *DiscordProvider) GetUserInfo(ctx context.Context, token *OAuthToken) (*OAuthUser, error) {
	logger.LogDebug(ctx, "[OAuth-Discord] GetUserInfo: fetching user info")

	req, err := http.NewRequestWithContext(ctx, "GET", "https://discord.com/api/v10/users/@me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	client := http.Client{
		Timeout: 5 * time.Second,
	}
	res, err := client.Do(req)
	if err != nil {
		logger.LogError(ctx, common.LogText("[OAuth-Discord] GetUserInfo error: %s", err.Error()))
		return nil, NewOAuthErrorWithRaw(common.NewMessage(msgConnectFailed, map[string]any{"provider": "Discord"}), err.Error())
	}
	defer res.Body.Close()

	logger.LogDebug(ctx, "[OAuth-Discord] GetUserInfo response status: %d", res.StatusCode)

	if res.StatusCode != http.StatusOK {
		logger.LogError(ctx, common.LogText("[OAuth-Discord] GetUserInfo failed: status=%d", res.StatusCode))
		return nil, NewOAuthError(common.NewMessage(msgGetUserFailed))
	}

	var discordUser discordUser
	err = common.DecodeJson(res.Body, &discordUser)
	if err != nil {
		logger.LogError(ctx, common.LogText("[OAuth-Discord] GetUserInfo decode error: %s", err.Error()))
		return nil, err
	}

	if discordUser.UID == "" || discordUser.ID == "" {
		logger.LogError(ctx, common.LogText("[OAuth-Discord] GetUserInfo failed: empty user fields"))
		return nil, NewOAuthError(common.NewMessage(msgUserInfoEmpty, map[string]any{"provider": "Discord"}))
	}

	logger.LogDebug(ctx, "[OAuth-Discord] GetUserInfo success: uid=%s, username=%s, name=%s", discordUser.UID, discordUser.ID, discordUser.Name)

	return &OAuthUser{
		ProviderUserID: discordUser.UID,
		Username:       discordUser.ID,
		DisplayName:    discordUser.Name,
	}, nil
}

func (p *DiscordProvider) IsUserIDTaken(providerUserID string) bool {
	return model.IsDiscordIdAlreadyTaken(providerUserID)
}

func (p *DiscordProvider) FillUserByProviderID(user *model.User, providerUserID string) error {
	user.DiscordId = providerUserID
	return user.FillUserByDiscordId()
}

func (p *DiscordProvider) SetProviderUserID(user *model.User, providerUserID string) {
	user.DiscordId = providerUserID
}

func (p *DiscordProvider) GetProviderPrefix() string {
	return "discord_"
}

// ProviderUserIDColumn returns the users-table column storing this provider's user ID.
func (p *DiscordProvider) ProviderUserIDColumn() string {
	return "discord_id"
}
