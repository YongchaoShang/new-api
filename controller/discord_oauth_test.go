package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	discordTestGuildA      = "111111111111111111"
	discordTestGuildB      = "222222222222222222"
	discordTestRoleA       = "333333333333333333"
	discordTestRoleB       = "444444444444444444"
	discordTestUser        = "555555555555555555"
	discordTestGuildRule   = `[{"guild_id":"` + discordTestGuildA + `","role_ids":[]}]`
	discordTestRoleRules   = `[{"guild_id":"` + discordTestGuildA + `","role_ids":["` + discordTestRoleA + `","` + discordTestRoleB + `"]},{"guild_id":"` + discordTestGuildB + `","role_ids":["` + discordTestRoleB + `"]}]`
	discordTestGuildList   = `[{"id":"` + discordTestGuildA + `"},{"id":"` + discordTestGuildB + `"}]`
	discordTestDenied      = "Your Discord account does not meet the server or role requirements"
	discordTestUnavailable = "Unable to verify Discord server access. Please start sign-in again"
)

// Tests replace only Discord's HTTP boundary and retain the real provider,
// admission logic, auth flow storage, and account/session operations.
type discordTestHTTPReply struct {
	method      string
	path        string
	status      int
	body        string
	beforeReply func()
}

type discordTestTransport struct {
	t       *testing.T
	replies []discordTestHTTPReply
	calls   int
}

func (transport *discordTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	transport.calls++
	if transport.calls > len(transport.replies) {
		transport.t.Errorf("unexpected Discord request: %s %s", request.Method, request.URL.Path)
		return nil, errors.New("unexpected Discord request")
	}
	reply := transport.replies[transport.calls-1]
	assert.Equal(transport.t, "https", request.URL.Scheme)
	assert.Equal(transport.t, "discord.com", request.URL.Host)
	assert.Equal(transport.t, reply.method, request.Method)
	assert.Equal(transport.t, reply.path, request.URL.RequestURI())
	if request.Method == http.MethodGet {
		assert.Equal(transport.t, "Bearer discord-test-access", request.Header.Get("Authorization"))
	}
	if reply.beforeReply != nil {
		reply.beforeReply()
	}
	return &http.Response{StatusCode: reply.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(reply.body)), Request: request}, nil
}

func installDiscordTestTransport(t *testing.T, replies ...discordTestHTTPReply) *discordTestTransport {
	t.Helper()
	previous := http.DefaultTransport
	transport := &discordTestTransport{t: t, replies: replies}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	return transport
}

func configureDiscordTest(t *testing.T, rules string) {
	t.Helper()
	previousSettings := *system_setting.GetDiscordSettings()
	previousRules, err := system_setting.GetDiscordGuildRulesSnapshot()
	require.NoError(t, err)
	encoded, err := common.Marshal(previousRules.Rules)
	require.NoError(t, err)
	previousAddress := system_setting.ServerAddress
	*system_setting.GetDiscordSettings() = system_setting.DiscordSettings{Enabled: true, ClientId: "discord-test-client", ClientSecret: "discord-test-secret"}
	system_setting.ServerAddress = "https://example.com"
	require.NoError(t, system_setting.LoadDiscordGuildRules(rules))
	t.Cleanup(func() {
		*system_setting.GetDiscordSettings() = previousSettings
		system_setting.ServerAddress = previousAddress
		require.NoError(t, system_setting.LoadDiscordGuildRules(string(encoded)))
	})
}

func TestDiscordGuildRulesRejectAccidentalRelaxation(t *testing.T) {
	for _, raw := range []string{
		"", "null", `{}`, `[null]`, `[{}]`,
		`[{"guild_id":"` + discordTestGuildA + `"}]`,
		`[{"guild_id":"` + discordTestGuildA + `","role_ids":null}]`,
		`[{"guild_id":"` + discordTestGuildA + `","role_ids":"` + discordTestRoleA + `"}]`,
		`[{"guild_id":"` + discordTestGuildA + `","role_ids":[],"roles":[]}]`,
		`[{"guild_id":111111111111111111,"role_ids":[]}]`,
		`[{"guild_id":"0","role_ids":[]}]`,
		`[{"guild_id":"18446744073709551616","role_ids":[]}]`,
		`[{"guild_id":"https://example.com","role_ids":[]}]`,
		`[{"guild_id":"` + discordTestGuildA + `","role_ids":[""]}]`,
		`[{"guild_id":"` + discordTestGuildA + `","role_ids":[333333333333333333]}]`,
		`[{"guild_id":"` + discordTestGuildA + `","role_ids":[]},{"guild_id":" ` + discordTestGuildA + ` ","role_ids":[]}]`,
		`[] {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := system_setting.ParseDiscordGuildRules(raw)
			assert.Error(t, err)
		})
	}
	rules, err := system_setting.ParseDiscordGuildRules(`[{"guild_id":" ` + discordTestGuildA + ` ","role_ids":[" ` + discordTestRoleA + ` ","` + discordTestRoleA + `","` + discordTestRoleB + `"]}]`)
	require.NoError(t, err)
	assert.Equal(t, []system_setting.DiscordGuildRule{{GuildID: discordTestGuildA, RoleIDs: []string{discordTestRoleA, discordTestRoleB}}}, rules)
	rules, err = system_setting.ParseDiscordGuildRules("[]")
	require.NoError(t, err)
	assert.Empty(t, rules)
}

func TestDiscordAuthorizationScopesFollowServerIntent(t *testing.T) {
	for _, test := range []struct {
		name, rules, intent, scope, prompt string
	}{
		{"unrestricted login", "[]", "login", "identify", ""},
		{"guild login", discordTestGuildRule, "login", "identify guilds", "consent"},
		{"role login", discordTestRoleRules, "login", "identify guilds guilds.members.read", "consent"},
		{"everyone only", `[{"guild_id":"` + discordTestGuildA + `","role_ids":["` + discordTestGuildA + `"]}]`, "login", "identify guilds", "consent"},
		{"binding", discordTestRoleRules, "bind", "identify", ""},
		{"verification", discordTestRoleRules, "verify", "identify", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			configureDiscordTest(t, test.rules)
			raw, err := oauth.DiscordAuthorizationURL("opaque-state", test.intent)
			require.NoError(t, err)
			parsed, err := url.Parse(raw)
			require.NoError(t, err)
			assert.Equal(t, "https", parsed.Scheme)
			assert.Equal(t, "discord.com", parsed.Host)
			assert.Equal(t, test.scope, parsed.Query().Get("scope"))
			assert.Equal(t, test.prompt, parsed.Query().Get("prompt"))
			assert.Equal(t, "https://example.com/oauth/discord", parsed.Query().Get("redirect_uri"))
			assert.Equal(t, "opaque-state", parsed.Query().Get("state"))
			assert.Equal(t, "code", parsed.Query().Get("response_type"))
		})
	}
}

func TestDiscordLoginAdmission(t *testing.T) {
	guilds := discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me/guilds?limit=200", status: http.StatusOK, body: discordTestGuildList}
	memberA := discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me/guilds/" + discordTestGuildA + "/member", status: http.StatusOK, body: `{"roles":["` + discordTestRoleA + `","` + discordTestRoleB + `","666666666666666666"]}`}
	memberB := discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me/guilds/" + discordTestGuildB + "/member", status: http.StatusOK, body: `{"roles":["` + discordTestRoleB + `"]}`}
	missingRole := memberA
	missingRole.body = `{"roles":["` + discordTestRoleA + `"]}`
	missingOtherRole := memberB
	missingOtherRole.body = `{"roles":["` + discordTestRoleA + `"]}`
	forbidden, unavailable, notFound, unauthorized, limited := memberA, memberA, memberA, memberA, memberA
	forbidden.status, unavailable.status, notFound.status, unauthorized.status, limited.status = 403, 503, 404, 401, 429
	limited.body = `{"retry_after":60,"global":true}`
	malformedMember := memberA
	malformedMember.body = `{}`
	malformedGuilds := guilds
	malformedGuilds.body = `[{"name":"missing id"}]`
	noGuilds := guilds
	noGuilds.body = `[]`
	for _, test := range []struct {
		name    string
		rules   string
		scope   string
		replies []discordTestHTTPReply
		message string
	}{
		{name: "empty configuration skips API", rules: "[]"},
		{name: "membership only", rules: discordTestGuildRule, scope: "guilds", replies: []discordTestHTTPReply{guilds}},
		{name: "extra roles and first match short circuit", replies: []discordTestHTTPReply{guilds, memberA}},
		{name: "second guild satisfies complete rule", replies: []discordTestHTTPReply{guilds, missingRole, memberB}},
		{name: "roles cannot combine across guilds", rules: `[{"guild_id":"` + discordTestGuildA + `","role_ids":["` + discordTestRoleA + `","` + discordTestRoleB + `"]},{"guild_id":"` + discordTestGuildB + `","role_ids":["` + discordTestRoleA + `","` + discordTestRoleB + `"]}]`, replies: []discordTestHTTPReply{guilds, missingRole, memberB}, message: discordTestDenied},
		{name: "not in any target guild", replies: []discordTestHTTPReply{noGuilds}, message: discordTestDenied},
		{name: "empty role rule needs no member scope", rules: discordTestRoleRules[:len(discordTestRoleRules)-1] + `,{"guild_id":"777777777777777777","role_ids":[]}]`, scope: "guilds", replies: []discordTestHTTPReply{{method: guilds.method, path: guilds.path, status: 200, body: `[{"id":"777777777777777777"}]`}}},
		{name: "everyone follows membership", rules: `[{"guild_id":"` + discordTestGuildA + `","role_ids":["` + discordTestGuildA + `"]}]`, scope: "guilds", replies: []discordTestHTTPReply{guilds}},
		{name: "missing guild scope", scope: "identify", message: discordTestUnavailable},
		{name: "missing member scope", scope: "identify guilds", replies: []discordTestHTTPReply{guilds}, message: discordTestUnavailable},
		{name: "403 candidate can be bypassed by independent match", replies: []discordTestHTTPReply{guilds, forbidden, memberB}},
		{name: "503 candidate can be bypassed by independent match", replies: []discordTestHTTPReply{guilds, unavailable, memberB}},
		{name: "unknown candidate prevents definite denial", replies: []discordTestHTTPReply{guilds, forbidden, missingOtherRole}, message: discordTestUnavailable},
		{name: "404 candidate is definite nonmembership", replies: []discordTestHTTPReply{guilds, notFound, missingOtherRole}, message: discordTestDenied},
		{name: "expired token stops remaining requests", replies: []discordTestHTTPReply{guilds, unauthorized}, message: discordTestUnavailable},
		{name: "429 stops without immediate retry", replies: []discordTestHTTPReply{guilds, limited}, message: discordTestUnavailable},
		{name: "malformed guild cannot imply nonmembership", replies: []discordTestHTTPReply{malformedGuilds}, message: discordTestUnavailable},
		{name: "missing roles are unknown", replies: []discordTestHTTPReply{guilds, malformedMember, missingOtherRole}, message: discordTestUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.rules == "" {
				test.rules = discordTestRoleRules
			}
			if test.scope == "" {
				test.scope = "identify guilds guilds.members.read"
			}
			configureDiscordTest(t, test.rules)
			transport := installDiscordTestTransport(t, test.replies...)
			err := (&oauth.DiscordProvider{}).CheckLoginAdmission(context.Background(), &oauth.OAuthToken{AccessToken: "discord-test-access", Scope: test.scope})
			if test.message == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.EqualError(t, err, test.message)
			}
			assert.Equal(t, len(test.replies), transport.calls)
		})
	}
}

func TestDiscordLoginAdmissionRejectsCorruptOrChangedRules(t *testing.T) {
	for _, scenario := range []string{"corrupt", "changed during check", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			configureDiscordTest(t, discordTestGuildRule)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			replies := []discordTestHTTPReply{}
			switch scenario {
			case "corrupt":
				require.Error(t, system_setting.LoadDiscordGuildRules(`[{"guild_id":"broken"}]`))
			case "changed during check":
				replies = append(replies, discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me/guilds?limit=200", status: 200, body: discordTestGuildList, beforeReply: func() {
					require.NoError(t, system_setting.LoadDiscordGuildRules(discordTestRoleRules))
				}})
			case "cancelled":
				cancel()
			}
			transport := installDiscordTestTransport(t, replies...)
			err := (&oauth.DiscordProvider{}).CheckLoginAdmission(ctx, &oauth.OAuthToken{AccessToken: "discord-test-access", Scope: "identify guilds"})
			require.Error(t, err)
			assert.EqualError(t, err, discordTestUnavailable)
			assert.Equal(t, len(replies), transport.calls)
		})
	}
}

func TestDiscordGuildRuleSaveFailureKeepsPersistedAndActiveRules(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	configureDiscordTest(t, "[]")
	common.OptionMapRWMutex.Lock()
	previous, existed := common.OptionMap["discord.guild_rules"]
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		defer common.OptionMapRWMutex.Unlock()
		if existed {
			common.OptionMap["discord.guild_rules"] = previous
		} else {
			delete(common.OptionMap, "discord.guild_rules")
		}
	})
	require.NoError(t, model.UpdateOption("discord.guild_rules", discordTestGuildRule))
	before, err := system_setting.GetDiscordGuildRulesSnapshot()
	require.NoError(t, err)
	failure := errors.New("injected Discord option persistence failure")
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("discord_option_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "options" {
			tx.AddError(failure)
		}
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Update().Remove("discord_option_failure") })
	assert.ErrorIs(t, model.UpdateOption("discord.guild_rules", "[]"), failure)
	var persisted model.Option
	require.NoError(t, model.DB.Where(map[string]any{"key": "discord.guild_rules"}).First(&persisted).Error)
	assert.JSONEq(t, discordTestGuildRule, persisted.Value)
	after, err := system_setting.GetDiscordGuildRulesSnapshot()
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Error(t, model.UpdateOption("discord.guild_rules", "null"))
}

func startDiscordTestFlow(t *testing.T, body, proof string, identity service.AuthIdentity) (string, *http.Cookie) {
	t.Helper()
	response := securityEnrollmentRequest(http.MethodPost, "/api/oauth/state", body, proof, identity, GenerateOAuthCode)
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			FlowToken        string `json:"flow_token"`
			AuthorizationURL string `json:"authorization_url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	require.NotEmpty(t, result.Data.FlowToken)
	parsed, err := url.Parse(result.Data.AuthorizationURL)
	require.NoError(t, err)
	assert.Equal(t, result.Data.FlowToken, parsed.Query().Get("state"))
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == discordOAuthStateCookie(result.Data.FlowToken) {
			assert.True(t, cookie.HttpOnly)
			assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
			return result.Data.FlowToken, cookie
		}
	}
	t.Fatal("missing browser-bound Discord state cookie")
	return "", nil
}

func discordTestCallback(state string, cookie *http.Cookie, identity service.AuthIdentity) *httptest.ResponseRecorder {
	return securityEnrollmentRequest(http.MethodGet, "/api/oauth/discord?state="+state+"&code=discord-code", "", "", identity, func(c *gin.Context) {
		c.Params = gin.Params{{Key: "provider", Value: "discord"}}
		if cookie != nil {
			c.Request.AddCookie(cookie)
		}
		HandleOAuth(c)
	})
}

func TestDiscordAdmissionPrecedesAccountAndSessionCreation(t *testing.T) {
	for _, scenario := range []string{"new user denied", "existing user denied", "client cannot change login intent", "registration disabled after admission", "disabled account after admission"} {
		t.Run(scenario, func(t *testing.T) {
			user, _ := setupSecurityEnrollmentTest(t)
			configureDiscordTest(t, discordTestGuildRule)
			previousRegister := common.RegisterEnabled
			common.RegisterEnabled = scenario != "registration disabled after admission"
			t.Cleanup(func() { common.RegisterEnabled = previousRegister })
			if scenario == "existing user denied" || scenario == "disabled account after admission" {
				require.NoError(t, model.DB.Model(user).Update("discord_id", discordTestUser).Error)
			}
			if scenario == "disabled account after admission" {
				require.NoError(t, model.DB.Model(user).Update("status", common.UserStatusDisabled).Error)
			}
			guilds, expectedMessage := `[]`, discordTestDenied
			if scenario == "registration disabled after admission" {
				guilds, expectedMessage = discordTestGuildList, "New user registration has been disabled by administrator"
			}
			if scenario == "disabled account after admission" {
				guilds, expectedMessage = discordTestGuildList, "User has been banned"
			}
			transport := installDiscordTestTransport(t,
				discordTestHTTPReply{method: http.MethodPost, path: "/api/v10/oauth2/token", status: 200, body: `{"access_token":"discord-test-access","token_type":"Bearer","scope":"identify guilds"}`},
				discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me", status: 200, body: `{"id":"` + discordTestUser + `","username":"discord-user"}`},
				discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me/guilds?limit=200", status: 200, body: guilds},
			)
			state, cookie := startDiscordTestFlow(t, `{"provider":"discord","intent":"login"}`, "", service.AuthIdentity{})
			callbackState := state
			if scenario == "client cannot change login intent" {
				callbackState += "&intent=bind"
			}
			response := discordTestCallback(callbackState, cookie, service.AuthIdentity{})
			var result securityEnrollmentResponse
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.False(t, result.Success)
			assert.Equal(t, expectedMessage, result.Message)
			assert.NotContains(t, response.Body.String(), "access_token")
			_, err := model.GetAuthFlow(state, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeOAuth})
			assert.ErrorIs(t, err, model.ErrAuthFlowConsumed)
			response = discordTestCallback(state, cookie, service.AuthIdentity{})
			assert.Equal(t, http.StatusForbidden, response.Code)
			assert.Equal(t, 3, transport.calls, "replay must not exchange another code")
			var users, sessions int64
			require.NoError(t, model.DB.Model(&model.User{}).Count(&users).Error)
			require.NoError(t, model.DB.Model(&model.UserSession{}).Count(&sessions).Error)
			assert.EqualValues(t, 1, users)
			assert.EqualValues(t, 1, sessions, "only the fixture's preexisting session remains")
		})
	}
}

func TestDiscordStateRejectsWrongBrowserProviderAndExpiry(t *testing.T) {
	for _, scenario := range []string{"missing cookie", "wrong cookie", "expired", "other provider"} {
		t.Run(scenario, func(t *testing.T) {
			setupSecurityEnrollmentTest(t)
			configureDiscordTest(t, discordTestGuildRule)
			state, cookie := startDiscordTestFlow(t, `{"provider":"discord","intent":"login"}`, "", service.AuthIdentity{})
			switch scenario {
			case "missing cookie":
				cookie = nil
			case "wrong cookie":
				cookie.Value = "other-browser-state"
			case "expired":
				require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("provider = ?", "discord").Update("expires_at", time.Now().Add(-time.Minute)).Error)
			case "other provider":
				require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("provider = ?", "discord").Update("provider", "github").Error)
			}
			transport := installDiscordTestTransport(t)
			response := discordTestCallback(state, cookie, service.AuthIdentity{})
			assert.Equal(t, http.StatusForbidden, response.Code)
			assert.Zero(t, transport.calls)
		})
	}
}

func TestDiscordBindAndVerifySkipGuildAdmission(t *testing.T) {
	for _, intent := range []string{"bind", "verify"} {
		t.Run(intent, func(t *testing.T) {
			user, identity := setupSecurityEnrollmentTest(t)
			configureDiscordTest(t, discordTestRoleRules)
			body, proof := `{"provider":"discord","intent":"bind"}`, ""
			if intent == "bind" {
				proof = issueSecurityEnrollmentProof(t, identity, service.VerificationOperation{Scope: service.VerificationScopeAccountBind, Context: []byte(`{"provider":"discord"}`)}, service.VerificationMethodPassword)
			} else {
				require.NoError(t, model.DB.Model(user).Updates(map[string]any{"password": "", "discord_id": discordTestUser}).Error)
				body = `{"provider":"discord","intent":"verify","scope":"2fa.setup"}`
			}
			state, cookie := startDiscordTestFlow(t, body, proof, identity)
			// Corruption after authorization must not apply login policy to binding
			// or verification, both of which retain their session/proof checks.
			require.Error(t, system_setting.LoadDiscordGuildRules("null"))
			transport := installDiscordTestTransport(t,
				discordTestHTTPReply{method: http.MethodPost, path: "/api/v10/oauth2/token", status: 200, body: `{"access_token":"discord-test-access","token_type":"Bearer","scope":"identify"}`},
				discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me", status: 200, body: `{"id":"` + discordTestUser + `","username":"discord-user"}`},
			)
			wrongSession := identity
			wrongSession.SessionID = "another-session"
			response := discordTestCallback(state, cookie, wrongSession)
			assert.Equal(t, http.StatusForbidden, response.Code)
			assert.Zero(t, transport.calls)
			response = discordTestCallback(state, cookie, identity)
			var result securityEnrollmentResponse
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success, response.Body.String())
			assert.Equal(t, 2, transport.calls)
			assert.NotContains(t, response.Body.String(), "access_token")
			if intent == "verify" {
				var issued service.SecurityProof
				require.NoError(t, common.Unmarshal(result.Data, &issued))
				_, err := service.ConsumeOperationProof(issued.ProofToken, identity, service.VerificationOperation{Scope: "2fa.setup"})
				require.NoError(t, err)
			}
			var stored model.User
			require.NoError(t, model.DB.First(&stored, user.Id).Error)
			assert.Equal(t, discordTestUser, stored.DiscordId)
			var users, sessions int64
			require.NoError(t, model.DB.Model(&model.User{}).Count(&users).Error)
			require.NoError(t, model.DB.Model(&model.UserSession{}).Count(&sessions).Error)
			assert.EqualValues(t, 1, users)
			assert.EqualValues(t, 1, sessions)
		})
	}
}

func TestDiscordAdmissionPreservesSuccessfulLoginAndSecondFactor(t *testing.T) {
	for _, scenario := range []string{"new account", "existing account", "second factor required"} {
		t.Run(scenario, func(t *testing.T) {
			user, _ := setupSecurityEnrollmentTest(t)
			configureDiscordTest(t, discordTestGuildRule)
			previousRegister := common.RegisterEnabled
			common.RegisterEnabled = true
			t.Cleanup(func() { common.RegisterEnabled = previousRegister })
			if scenario != "new account" {
				require.NoError(t, model.DB.Model(user).Update("discord_id", discordTestUser).Error)
			}
			if scenario == "second factor required" {
				require.NoError(t, model.DB.Create(&model.TwoFA{UserId: user.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}).Error)
			}
			transport := installDiscordTestTransport(t,
				discordTestHTTPReply{method: http.MethodPost, path: "/api/v10/oauth2/token", status: 200, body: `{"access_token":"discord-test-access","token_type":"Bearer","scope":"identify guilds"}`},
				discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me", status: 200, body: `{"id":"` + discordTestUser + `","username":"discord-user"}`},
				discordTestHTTPReply{method: http.MethodGet, path: "/api/v10/users/@me/guilds?limit=200", status: 200, body: discordTestGuildList},
			)
			state, cookie := startDiscordTestFlow(t, `{"provider":"discord","intent":"login"}`, "", service.AuthIdentity{})
			response := discordTestCallback(state, cookie, service.AuthIdentity{})
			var result struct {
				Success bool `json:"success"`
				Data    struct {
					AccessToken         string `json:"access_token"`
					RequireVerification bool   `json:"require_verification"`
					FlowToken           string `json:"flow_token"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success, response.Body.String())
			assert.Equal(t, 3, transport.calls)
			var users, sessions int64
			require.NoError(t, model.DB.Model(&model.User{}).Count(&users).Error)
			require.NoError(t, model.DB.Model(&model.UserSession{}).Count(&sessions).Error)
			expectedUsers := int64(1)
			if scenario == "new account" {
				expectedUsers = 2
			}
			assert.Equal(t, expectedUsers, users)
			if scenario == "second factor required" {
				assert.True(t, result.Data.RequireVerification)
				assert.NotEmpty(t, result.Data.FlowToken)
				assert.Empty(t, result.Data.AccessToken)
				assert.EqualValues(t, 1, sessions)
			} else {
				assert.False(t, result.Data.RequireVerification)
				assert.NotEmpty(t, result.Data.AccessToken)
				assert.EqualValues(t, 2, sessions)
			}
		})
	}
}
