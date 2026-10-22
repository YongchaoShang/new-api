package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProcessChannelErrorUsesSnapshotWithoutLeakingChannelMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRules, err := kitutil.CompileRedactionKeywords(operation_setting.GetLogSetting().RedactionKeywords)
	require.NoError(t, err)
	kitutil.SetRedactionKeywords(nil)
	t.Cleanup(func() { kitutil.SetRedactionKeywords(previousRules) })
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	previousErrorLogEnabled := constant.ErrorLogEnabled

	database, err := gorm.Open(sqlite.Open(t.TempDir()+"/relay-error-log.db"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	// Admin exports look up channels while log rows still hold a connection.
	// A file-backed database lets both connections see the same fixture.
	sqlDB.SetMaxOpenConns(2)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Log{}))
	model.DB, model.LOG_DB = database, database
	common.RedisEnabled = false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	constant.ErrorLogEnabled = true
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		constant.ErrorLogEnabled = previousErrorLogEnabled
		require.NoError(t, sqlDB.Close())
	})

	require.NoError(t, database.Create(&model.User{Id: 7, Username: "log-owner", Group: "default"}).Error)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 7)
	ctx.Set("username", "log-owner")
	ctx.Set("token_name", "test-token")
	ctx.Set("token_id", 11)
	ctx.Set("original_model", "gpt-test")
	ctx.Set("group", "default")
	ctx.Set("channel_id", 202)
	ctx.Set("channel_name", "mutable-context-channel")
	ctx.Set("channel_type", 9)
	ctx.Set("use_channel", []string{"101"})
	common.SetContextKey(ctx, constant.ContextKeyRequestStartTime, time.Now().Add(-time.Second))

	channelSnapshot := types.ChannelError{
		ChannelId:   101,
		ChannelType: 1,
		ChannelName: "snapshot-channel",
		AutoBan:     false,
	}
	apiErr := types.NewOpenAIError(errors.New("upstream failed"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)

	processChannelError(ctx, channelSnapshot, apiErr, nil)

	var stored model.Log
	require.NoError(t, database.First(&stored).Error)
	assert.Equal(t, channelSnapshot.ChannelId, stored.ChannelId)
	storedOther, err := common.StrToMap(stored.Other)
	require.NoError(t, err)
	assert.Equal(t, float64(http.StatusBadGateway), storedOther["status_code"])
	for _, key := range []string{"channel_id", "channel_name", "channel_type"} {
		assert.NotContains(t, storedOther, key)
	}
	adminInfo, ok := storedOther["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"101"}, adminInfo["use_channel"])

	logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeError}, 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, logs, 1)
	assert.Equal(t, channelSnapshot.ChannelId, logs[0].ChannelId)
	assert.Empty(t, logs[0].ChannelName)
	userOther, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	assert.NotContains(t, userOther, "admin_info")
	for _, key := range []string{"channel_id", "channel_name", "channel_type"} {
		assert.NotContains(t, userOther, key)
	}

	t.Run("keyword redaction is saved for every viewer without rewriting history", func(t *testing.T) {
		previousSetting := *operation_setting.GetLogSetting()
		previousRules, err := kitutil.CompileRedactionKeywords(previousSetting.RedactionKeywords)
		require.NoError(t, err)
		previousMemoryCache := common.MemoryCacheEnabled
		key := operation_setting.LogRedactionKeywordsOptionKey
		common.OptionMapRWMutex.Lock()
		previousValue, existed := common.OptionMap[key]
		previousNilOptions := common.OptionMap == nil
		if previousNilOptions {
			common.OptionMap = make(map[string]string)
		}
		common.OptionMapRWMutex.Unlock()
		t.Cleanup(func() {
			*operation_setting.GetLogSetting() = previousSetting
			kitutil.SetRedactionKeywords(previousRules)
			common.MemoryCacheEnabled = previousMemoryCache
			common.OptionMapRWMutex.Lock()
			if previousNilOptions {
				common.OptionMap = nil
			} else if existed {
				common.OptionMap[key] = previousValue
			} else {
				delete(common.OptionMap, key)
			}
			common.OptionMapRWMutex.Unlock()
		})
		common.MemoryCacheEnabled = false
		operation_setting.GetLogSetting().UserExportEnabled = true
		operation_setting.GetLogSetting().MaxRangeDays = 365
		require.NoError(t, database.AutoMigrate(&model.Option{}, &model.Channel{}))
		require.NoError(t, database.Create(&model.Channel{Id: channelSnapshot.ChannelId, Name: channelSnapshot.ChannelName}).Error)
		require.NoError(t, model.UpdateOption(key, `["upstream","VendorAlpha","502"]`))
		ctx.Set(common.RequestIdKey, "keyword-redaction")
		message := "upstream VENDORALPHA returned 502"
		maskedErr := types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)
		processChannelError(ctx, channelSnapshot, maskedErr, nil)
		want := "status_code=502, *** *** returned ***"
		var redacted model.Log
		require.NoError(t, database.Where("request_id = ?", "keyword-redaction").First(&redacted).Error)
		assert.Equal(t, want, redacted.Content)
		var metadata map[string]any
		require.NoError(t, common.UnmarshalJsonStr(redacted.Other, &metadata))
		assert.EqualValues(t, http.StatusBadGateway, metadata["status_code"])
		assert.Equal(t, string(types.ErrorTypeOpenAIError), metadata["error_type"])
		assert.Equal(t, string(types.ErrorCodeBadResponseStatusCode), metadata["error_code"])
		assert.Equal(t, message, maskedErr.Error())
		assert.Equal(t, http.StatusBadGateway, maskedErr.StatusCode)
		assert.Equal(t, "*** *** returned ***", maskedErr.ToOpenAIError().Message)
		assert.Equal(t, "*** *** returned ***", maskedErr.ToClaudeError().Message)
		claudeErr := types.WithClaudeError(types.ClaudeError{Type: "overloaded_error", Message: message}, http.StatusBadGateway)
		assert.Equal(t, "*** *** returned ***", claudeErr.ToClaudeError().Message)
		assert.Equal(t, "overloaded_error", claudeErr.ToClaudeError().Type)
		assert.Equal(t, message, claudeErr.Error())
		assert.Equal(t, http.StatusBadGateway, claudeErr.StatusCode)

		response := httptest.NewRecorder()
		responseContext, _ := gin.CreateTestContext(response)
		responseContext.JSON(maskedErr.StatusCode, gin.H{"error": maskedErr.ToOpenAIError()})
		assert.Equal(t, http.StatusBadGateway, response.Code)
		var responseBody struct {
			Error types.OpenAIError `json:"error"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &responseBody))
		assert.Equal(t, "*** *** returned ***", responseBody.Error.Message)
		assert.Equal(t, string(types.ErrorCodeBadResponseStatusCode), responseBody.Error.Code)

		query := fmt.Sprintf("start_timestamp=%d&end_timestamp=%d&request_id=keyword-redaction", common.GetTimestamp()-3600, common.GetTimestamp()+3600)
		for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
			for _, all := range []bool{false, true} {
				if all && role == common.RoleCommonUser {
					continue
				}
				list, export := gin.HandlerFunc(GetUserLogs), gin.HandlerFunc(ExportUserLogs)
				if all {
					list, export = GetAllLogs, ExportAllLogs
				}
				listed := callLogHandler(t, list, 7, role, query)[0]
				require.Equal(t, true, listed["success"])
				data := listed["data"].(map[string]any)
				items := data["items"].([]any)
				require.Len(t, items, 1)
				assert.Equal(t, want, items[0].(map[string]any)["content"], "role=%d all=%t", role, all)
				if all {
					assert.Equal(t, channelSnapshot.ChannelName, items[0].(map[string]any)["channel_name"])
				}
				exported := callLogHandler(t, export, 7, role, query)
				require.Len(t, exported, 2)
				assert.Equal(t, true, exported[1]["success"])
				assert.Equal(t, want, exported[0]["content"], "role=%d all=%t", role, all)
				if all {
					assert.Equal(t, channelSnapshot.ChannelName, exported[0]["channel_name"])
				}
			}
		}

		require.NoError(t, model.UpdateOption(key, `["different-keyword"]`))
		ctx.Set(common.RequestIdKey, "after-redaction-changed")
		changedErr := types.NewOpenAIError(errors.New("different-keyword VendorAlpha"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)
		processChannelError(ctx, channelSnapshot, changedErr, nil)
		var changed model.Log
		require.NoError(t, database.Where("request_id = ?", "after-redaction-changed").First(&changed).Error)
		assert.Equal(t, "status_code=502, *** VendorAlpha", changed.Content)
		require.NoError(t, model.UpdateOption(key, `[]`))
		var historical model.Log
		require.NoError(t, database.First(&historical, stored.Id).Error)
		assert.Equal(t, stored.Content, historical.Content, "earlier raw history is not retroactively redacted")
		redacted = model.Log{}
		require.NoError(t, database.Where("request_id = ?", "keyword-redaction").First(&redacted).Error)
		assert.Equal(t, want, redacted.Content, "saved masks do not recover after clearing keywords")
		ctx.Set(common.RequestIdKey, "after-redaction-cleared")
		processChannelError(ctx, channelSnapshot, maskedErr, nil)
		var cleared model.Log
		require.NoError(t, database.Where("request_id = ?", "after-redaction-cleared").First(&cleared).Error)
		assert.Equal(t, "status_code=502, "+message, cleared.Content)
	})
}

func TestUserLogRedactionPreservesInternalDecisions(t *testing.T) {
	previousRules, err := kitutil.CompileRedactionKeywords(operation_setting.GetLogSetting().RedactionKeywords)
	require.NoError(t, err)
	previousAutomaticDisable := common.AutomaticDisableChannelEnabled
	previousDisableKeywords := operation_setting.AutomaticDisableKeywords
	previousDisableCodes := operation_setting.AutomaticDisableStatusCodeRanges
	previousRetryCodes := operation_setting.AutomaticRetryStatusCodeRanges
	t.Cleanup(func() {
		kitutil.SetRedactionKeywords(previousRules)
		common.AutomaticDisableChannelEnabled = previousAutomaticDisable
		operation_setting.AutomaticDisableKeywords = previousDisableKeywords
		operation_setting.AutomaticDisableStatusCodeRanges = previousDisableCodes
		operation_setting.AutomaticRetryStatusCodeRanges = previousRetryCodes
	})
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableKeywords = []string{"vendoralpha"}
	operation_setting.AutomaticDisableStatusCodeRanges = nil
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 502, End: 502}}
	rules, err := kitutil.CompileRedactionKeywords([]string{"VendorAlpha", service.CSAMViolationMarker})
	require.NoError(t, err)
	kitutil.SetRedactionKeywords(rules)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	upstreamErr := types.NewOpenAIError(errors.New("VendorAlpha failed"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)
	assert.Equal(t, "*** failed", upstreamErr.ToOpenAIError().Message)
	assert.True(t, service.ShouldDisableChannel(upstreamErr), "disable keywords inspect the original message")
	assert.True(t, service.ShouldRetryRelayError(ctx, upstreamErr, 1))
	assert.Equal(t, "VendorAlpha failed", upstreamErr.Error())
	violation := types.NewOpenAIError(errors.New(service.CSAMViolationMarker), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)
	assert.Equal(t, "***", violation.ToOpenAIError().Message)
	assert.True(t, service.HasCSAMViolationMarker(violation))
	normalized := service.NormalizeViolationFeeError(violation)
	assert.Equal(t, types.ErrorCodeViolationFeeGrokCSAM, normalized.GetErrorCode())
	assert.Equal(t, http.StatusBadRequest, normalized.StatusCode)
	assert.True(t, types.IsSkipRetryError(normalized))
	assert.False(t, service.ShouldRetryRelayError(ctx, normalized, 1))
	assert.Equal(t, service.CSAMViolationMarker, violation.Error())
}
