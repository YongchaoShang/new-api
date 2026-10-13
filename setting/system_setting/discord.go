package system_setting

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

const DiscordGuildRulesOptionKey = "discord.guild_rules"

var ErrDiscordGuildRulesInvalid = errors.New("Invalid Discord server or role rules")

type DiscordGuildRule struct {
	GuildID string   `json:"guild_id"`
	RoleIDs []string `json:"role_ids"`
}

type DiscordGuildRulesSnapshot struct {
	Rules   []DiscordGuildRule
	Version uint64
}

var discordGuildRules = struct {
	sync.RWMutex
	raw      string
	snapshot DiscordGuildRulesSnapshot
	err      error
}{raw: "[]", snapshot: DiscordGuildRulesSnapshot{Rules: []DiscordGuildRule{}, Version: 1}}

// ParseDiscordGuildRules accepts an explicit array only. Missing fields and
// malformed stored data must never silently remove admission restrictions.
func ParseDiscordGuildRules(raw string) ([]DiscordGuildRule, error) {
	var entries []map[string]common.RawMessage
	if err := common.UnmarshalJsonStr(raw, &entries); err != nil || entries == nil {
		return nil, ErrDiscordGuildRulesInvalid
	}
	rules := make([]DiscordGuildRule, 0, len(entries))
	guilds := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		guildJSON, hasGuild := entry["guild_id"]
		rolesJSON, hasRoles := entry["role_ids"]
		if len(entry) != 2 || !hasGuild || !hasRoles {
			return nil, ErrDiscordGuildRulesInvalid
		}
		var guildID string
		if err := common.Unmarshal(guildJSON, &guildID); err != nil {
			return nil, ErrDiscordGuildRulesInvalid
		}
		guildID, err := NormalizeDiscordSnowflake(guildID)
		if err != nil {
			return nil, err
		}
		if _, duplicate := guilds[guildID]; duplicate {
			return nil, ErrDiscordGuildRulesInvalid
		}
		guilds[guildID] = struct{}{}
		var roleIDs []string
		if err := common.Unmarshal(rolesJSON, &roleIDs); err != nil || roleIDs == nil {
			return nil, ErrDiscordGuildRulesInvalid
		}
		roles := make([]string, 0, len(roleIDs))
		seenRoles := make(map[string]struct{}, len(roleIDs))
		for _, roleID := range roleIDs {
			roleID, err := NormalizeDiscordSnowflake(roleID)
			if err != nil {
				return nil, err
			}
			if _, duplicate := seenRoles[roleID]; duplicate {
				continue
			}
			seenRoles[roleID] = struct{}{}
			roles = append(roles, roleID)
		}
		rules = append(rules, DiscordGuildRule{GuildID: guildID, RoleIDs: roles})
	}
	return rules, nil
}

// NormalizeDiscordSnowflake preserves the full unsigned 64-bit ID as text.
func NormalizeDiscordSnowflake(value string) (string, error) {
	value = strings.TrimSpace(value)
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != value {
		return "", ErrDiscordGuildRulesInvalid
	}
	return value, nil
}

// LoadDiscordGuildRules publishes invalid data as an error, not an empty rule
// set. The revision changes even if a later update restores an earlier value.
func LoadDiscordGuildRules(raw string) error {
	rules, err := ParseDiscordGuildRules(raw)
	discordGuildRules.Lock()
	defer discordGuildRules.Unlock()
	if raw != discordGuildRules.raw {
		discordGuildRules.snapshot.Version++
	}
	discordGuildRules.raw = raw
	discordGuildRules.snapshot.Rules = rules
	discordGuildRules.err = err
	return err
}

func GetDiscordGuildRulesSnapshot() (DiscordGuildRulesSnapshot, error) {
	discordGuildRules.RLock()
	defer discordGuildRules.RUnlock()
	if discordGuildRules.err != nil {
		return DiscordGuildRulesSnapshot{}, discordGuildRules.err
	}
	snapshot := discordGuildRules.snapshot
	snapshot.Rules = slices.Clone(snapshot.Rules)
	for i := range snapshot.Rules {
		snapshot.Rules[i].RoleIDs = slices.Clone(snapshot.Rules[i].RoleIDs)
	}
	return snapshot, nil
}

func DiscordGuildRulesVersionCurrent(version uint64) bool {
	discordGuildRules.RLock()
	defer discordGuildRules.RUnlock()
	return discordGuildRules.err == nil && discordGuildRules.snapshot.Version == version
}

type DiscordSettings struct {
	Enabled      bool   `json:"enabled"`
	ClientId     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// 默认配置
var defaultDiscordSettings = DiscordSettings{}

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("discord", &defaultDiscordSettings)
}

func GetDiscordSettings() *DiscordSettings {
	return &defaultDiscordSettings
}
