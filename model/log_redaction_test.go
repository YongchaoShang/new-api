package model

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestUserLogRedactionDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ name, env string }{
		{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect.name {
			case "sqlite":
				driver = sqlite.Open(t.TempDir() + "/redaction.db")
			case "mysql", "postgres":
				dsn := os.Getenv(dialect.env)
				if dsn == "" {
					t.Skip(dialect.env + " is not configured")
				}
				if dialect.name == "mysql" {
					driver = mysql.Open(dsn)
				} else {
					driver = postgres.Open(dsn)
				}
			}
			prefix := fmt.Sprintf("redaction_%d_", time.Now().UnixNano())
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: prefix}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&Option{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&Option{})) })
			var version string
			versionSQL := "SELECT version()"
			if dialect.name == "sqlite" {
				versionSQL = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
			t.Logf("primary database: %s %s", dialect.name, version)

			previousDB, previousType := DB, common.MainDatabaseType()
			previousSetting := *operation_setting.GetLogSetting()
			previousRules, err := kitutil.CompileRedactionKeywords(previousSetting.RedactionKeywords)
			require.NoError(t, err)
			previousPolicy := CurrentRequestPolicy()
			previousDiscord, err := system_setting.GetDiscordGuildRulesSnapshot()
			require.NoError(t, err)
			discordJSON, err := common.Marshal(previousDiscord.Rules)
			require.NoError(t, err)
			common.OptionMapRWMutex.Lock()
			previousOptions := common.OptionMap
			common.OptionMap = maps.Clone(previousOptions)
			if common.OptionMap == nil {
				common.OptionMap = make(map[string]string)
			}
			common.OptionMapRWMutex.Unlock()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect.name))
			t.Cleanup(func() {
				DB = previousDB
				common.SetMainDatabaseType(previousType)
				*operation_setting.GetLogSetting() = previousSetting
				kitutil.SetRedactionKeywords(previousRules)
				requestPolicySnapshot.Store(previousPolicy)
				require.NoError(t, system_setting.LoadDiscordGuildRules(string(discordJSON)))
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
			})
			// Reload must not change the unrelated admission configuration.
			require.NoError(t, db.Create(&Option{Key: system_setting.DiscordGuildRulesOptionKey, Value: string(discordJSON)}).Error)

			key := operation_setting.LogRedactionKeywordsOptionKey
			require.NoError(t, UpdateOption(key, `[" VendorAlpha ","vendoralpha","", " a.b "]`))
			assert.JSONEq(t, `["VendorAlpha","a.b"]`, requireOptionValue(t, db, key))
			assert.Equal(t, []string{"VendorAlpha", "a.b"}, operation_setting.GetLogSetting().RedactionKeywords)
			assert.Equal(t, "*** *** aXb", common.MaskSensitiveInfo("VENDORALPHA a.b aXb"))

			for _, invalid := range []string{`[`, `null`, `{}`, `"VendorAlpha"`, `[42]`, `[true]`, `["VendorAlpha",null]`, `[["VendorAlpha"]]`} {
				assert.Error(t, UpdateOption(key, invalid), "value: %s", invalid)
				assert.JSONEq(t, `["VendorAlpha","a.b"]`, requireOptionValue(t, db, key))
				assert.Equal(t, "*** Beta", common.MaskSensitiveInfo("VendorAlpha Beta"))
			}

			t.Run("failed save preserves effective rules", func(t *testing.T) {
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("redaction_fail", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Option" {
						tx.AddError(errors.New("redaction fixture rejects the write"))
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("redaction_fail")) })
				assert.Error(t, UpdateOption(key, `["Beta"]`))
				assert.JSONEq(t, `["VendorAlpha","a.b"]`, requireOptionValue(t, db, key))
				assert.Equal(t, []string{"VendorAlpha", "a.b"}, operation_setting.GetLogSetting().RedactionKeywords)
				common.OptionMapRWMutex.RLock()
				cached := common.OptionMap[key]
				common.OptionMapRWMutex.RUnlock()
				assert.JSONEq(t, `["VendorAlpha","a.b"]`, cached)
				assert.Equal(t, "*** Beta", common.MaskSensitiveInfo("VendorAlpha Beta"))
			})

			// Simulate a new process's empty keyword runtime, then use the same
			// one-shot loader used by startup and periodic synchronization.
			kitutil.SetRedactionKeywords(nil)
			operation_setting.GetLogSetting().RedactionKeywords = []string{}
			common.OptionMapRWMutex.Lock()
			delete(common.OptionMap, key)
			common.OptionMapRWMutex.Unlock()
			loadOptionsFromDatabase()
			loadOptionsFromDatabase()
			assert.Equal(t, []string{"VendorAlpha", "a.b"}, operation_setting.GetLogSetting().RedactionKeywords)
			assert.Equal(t, "***", common.MaskSensitiveInfo("vendoralpha"))

			require.NoError(t, db.Model(&Option{}).Where(&Option{Key: key}).Update("value", `[null]`).Error)
			loadOptionsFromDatabase()
			assert.Equal(t, "***", common.MaskSensitiveInfo("vendoralpha"), "invalid stored data cannot replace valid active rules")
			require.NoError(t, UpdateOption(key, `[]`))
			loadOptionsFromDatabase()
			loadOptionsFromDatabase()
			assert.JSONEq(t, `[]`, requireOptionValue(t, db, key))
			assert.Empty(t, operation_setting.GetLogSetting().RedactionKeywords)
			assert.Equal(t, "VendorAlpha https://***.com/***", common.MaskSensitiveInfo("VendorAlpha https://private.example.com/path", "https://private.example.com"))
		})
	}
}
