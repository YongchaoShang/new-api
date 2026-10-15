package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func configureCheckinTest(t *testing.T, enabled bool, allowedQuota *int) {
	t.Helper()
	registered := config.GlobalConfig.Get("checkin_setting").(*operation_setting.CheckinSetting)
	common.OptionMapRWMutex.Lock()
	previous := *registered
	*registered = operation_setting.CheckinSetting{Enabled: enabled, MinQuota: 25, MaxQuota: 25, AllowedQuota: allowedQuota}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		*registered = previous
		common.OptionMapRWMutex.Unlock()
	})
}

func createCheckinTestUser(t *testing.T, db *gorm.DB, quota int) model.User {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.User{}).Count(&count).Error)
	name := fmt.Sprintf("checkin-%d", count+1)
	user := model.User{Username: name, AffCode: name, Quota: quota, AuthVersion: 1}
	require.NoError(t, db.Create(&user).Error)
	return user
}

type checkinTestResponse struct {
	Success    bool   `json:"success"`
	MessageKey string `json:"message_key"`
	Data       struct {
		QuotaAwarded int `json:"quota_awarded"`
	} `json:"data"`
}

func requestCheckinTest(t *testing.T, userID int) checkinTestResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/user/checkin", nil)
	ctx.Set("id", userID)
	DoCheckin(ctx)
	var response checkinTestResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return response
}

func TestCheckinDatabaseMatrix(t *testing.T) {
	const exceeded = "Your current balance exceeds the check-in eligibility quota. Please avoid repeated check-ins."
	const invalid = "Check-in eligibility quota must be empty or a non-negative safe integer"
	const optionKey = "checkin_setting.allowed_quota"
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			db := modelManagementDB(t, dialect.kind, os.Getenv(dialect.env))
			require.NoError(t, db.AutoMigrate(&model.Checkin{}, &model.Log{}))
			zero, limit := 0, 100

			for _, tc := range []struct {
				name    string
				allowed *int
				balance int
				accept  bool
			}{
				{"unset", nil, 1000, true},
				{"below", &limit, 99, true},
				{"equal_full_reward", &limit, 100, true},
				{"above", &limit, 101, false},
				{"zero_negative_balance", &zero, -1, true},
				{"zero_zero_balance", &zero, 0, true},
				{"zero_positive_balance", &zero, 1, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					configureCheckinTest(t, true, tc.allowed)
					user := createCheckinTestUser(t, db, tc.balance)
					response := requestCheckinTest(t, user.Id)
					require.Equal(t, tc.accept, response.Success)
					require.NoError(t, db.First(&user, user.Id).Error)
					stats, err := model.GetUserCheckinStats(user.Id, time.Now().Format("2006-01"))
					require.NoError(t, err)
					var logs int64
					require.NoError(t, db.Model(&model.Log{}).Where("user_id = ?", user.Id).Count(&logs).Error)
					assert.Equal(t, tc.accept, stats["checked_in_today"])
					if tc.accept {
						assert.Equal(t, 25, response.Data.QuotaAwarded)
						assert.Equal(t, tc.balance+25, user.Quota)
						assert.Equal(t, int64(1), stats["total_checkins"])
						assert.Equal(t, int64(25), stats["total_quota"])
						assert.Equal(t, int64(1), logs)
					} else {
						assert.Equal(t, exceeded, response.MessageKey)
						assert.Equal(t, tc.balance, user.Quota)
						assert.Equal(t, int64(0), stats["total_checkins"])
						assert.Equal(t, int64(0), stats["total_quota"])
						assert.Zero(t, logs)
						// A denied attempt does not consume the user's daily opportunity.
						require.NoError(t, db.Model(&user).Update("quota", *tc.allowed).Error)
						retry := requestCheckinTest(t, user.Id)
						require.True(t, retry.Success)
						require.NoError(t, db.First(&user, user.Id).Error)
						assert.Equal(t, *tc.allowed+25, user.Quota)
					}
				})
			}

			t.Run("disabled_and_missing_user", func(t *testing.T) {
				configureCheckinTest(t, false, nil)
				user := createCheckinTestUser(t, db, 0)
				response := requestCheckinTest(t, user.Id)
				assert.False(t, response.Success)
				assert.Equal(t, "Check-in feature is not enabled", response.MessageKey)
				for _, allowed := range []*int{nil, &limit} {
					t.Run(fmt.Sprint(allowed == nil), func(t *testing.T) {
						configureCheckinTest(t, true, allowed)
						response := requestCheckinTest(t, -1)
						assert.False(t, response.Success)
						var count int64
						require.NoError(t, db.Model(&model.Checkin{}).Where("user_id = ?", -1).Count(&count).Error)
						assert.Zero(t, count)
					})
				}
			})

			t.Run("daily_once_and_eligibility_precedes_duplicate", func(t *testing.T) {
				configureCheckinTest(t, true, &limit)
				user := createCheckinTestUser(t, db, 0)
				require.True(t, requestCheckinTest(t, user.Id).Success)
				duplicate := requestCheckinTest(t, user.Id)
				assert.False(t, duplicate.Success)
				assert.Equal(t, "Already checked in today", duplicate.MessageKey)
				require.NoError(t, db.Model(&user).Update("quota", limit+1).Error)
				assert.Equal(t, exceeded, requestCheckinTest(t, user.Id).MessageKey)
				var count int64
				require.NoError(t, db.Model(&model.Checkin{}).Where("user_id = ?", user.Id).Count(&count).Error)
				assert.Equal(t, int64(1), count)
			})

			t.Run("zero_reward_records_checkin_without_changing_balance", func(t *testing.T) {
				configureCheckinTest(t, true, &limit)
				registered := config.GlobalConfig.Get("checkin_setting").(*operation_setting.CheckinSetting)
				common.OptionMapRWMutex.Lock()
				registered.MinQuota, registered.MaxQuota = 0, 0
				common.OptionMapRWMutex.Unlock()
				user := createCheckinTestUser(t, db, limit)
				response := requestCheckinTest(t, user.Id)
				require.True(t, response.Success)
				assert.Zero(t, response.Data.QuotaAwarded)
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.Equal(t, limit, user.Quota)
				stats, err := model.GetUserCheckinStats(user.Id, time.Now().Format("2006-01"))
				require.NoError(t, err)
				assert.Equal(t, true, stats["checked_in_today"])
				assert.Equal(t, int64(1), stats["total_checkins"])
				assert.Equal(t, int64(0), stats["total_quota"])
				assert.Equal(t, "Already checked in today", requestCheckinTest(t, user.Id).MessageKey)
				// Even with no reward to update, a nonexistent account must not gain a record.
				assert.False(t, requestCheckinTest(t, -1).Success)
				var count int64
				require.NoError(t, db.Model(&model.Checkin{}).Where("user_id = ?", -1).Count(&count).Error)
				assert.Zero(t, count)
			})

			t.Run("concurrent_requests_award_once", func(t *testing.T) {
				configureCheckinTest(t, true, &limit)
				user := createCheckinTestUser(t, db, 90)
				start := make(chan struct{})
				results := make(chan error, 2)
				var ready, workers sync.WaitGroup
				ready.Add(2)
				for range 2 {
					workers.Go(func() {
						ready.Done()
						<-start
						_, err := model.UserCheckin(user.Id)
						results <- err
					})
				}
				ready.Wait()
				close(start)
				workers.Wait()
				close(results)
				var successes int
				for err := range results {
					if err == nil {
						successes++
					}
				}
				assert.Equal(t, 1, successes)
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.Equal(t, 115, user.Quota)
				var count int64
				require.NoError(t, db.Model(&model.Checkin{}).Where("user_id = ?", user.Id).Count(&count).Error)
				assert.Equal(t, int64(1), count)
			})

			for _, stage := range []string{"read_balance", "create_record", "update_balance"} {
				t.Run("rollback/"+stage, func(t *testing.T) {
					configureCheckinTest(t, true, &limit)
					user := createCheckinTestUser(t, db, 90)
					callback := func(tx *gorm.DB) {
						table := "users"
						if stage == "create_record" {
							table = "checkins"
						}
						if tx.Statement.Table == table {
							tx.AddError(errors.New("injected check-in storage failure"))
						}
					}
					var remove func() error
					switch stage {
					case "read_balance":
						require.NoError(t, db.Callback().Query().Before("gorm:query").Register("checkin_failure", callback))
						remove = func() error { return db.Callback().Query().Remove("checkin_failure") }
					case "create_record":
						require.NoError(t, db.Callback().Create().Before("gorm:create").Register("checkin_failure", callback))
						remove = func() error { return db.Callback().Create().Remove("checkin_failure") }
					case "update_balance":
						require.NoError(t, db.Callback().Update().Before("gorm:update").Register("checkin_failure", callback))
						remove = func() error { return db.Callback().Update().Remove("checkin_failure") }
					}
					t.Cleanup(func() { require.NoError(t, remove()) })
					response := requestCheckinTest(t, user.Id)
					require.NoError(t, remove())
					assert.False(t, response.Success)
					require.NoError(t, db.First(&user, user.Id).Error)
					assert.Equal(t, 90, user.Quota)
					var count int64
					require.NoError(t, db.Model(&model.Checkin{}).Where("user_id = ?", user.Id).Count(&count).Error)
					assert.Zero(t, count)
					require.NoError(t, db.Model(&model.Log{}).Where("user_id = ?", user.Id).Count(&count).Error)
					assert.Zero(t, count)
					require.True(t, requestCheckinTest(t, user.Id).Success)
				})
			}

			t.Run("database_balance_overrides_cache", func(t *testing.T) {
				configureCheckinTest(t, true, &limit)
				server := miniredis.RunT(t)
				previousRDB, previousRedis := common.RDB, common.RedisEnabled
				common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
				common.RedisEnabled = true
				t.Cleanup(func() {
					require.NoError(t, common.RDB.Close())
					common.RDB, common.RedisEnabled = previousRDB, previousRedis
				})
				for _, tc := range []struct {
					balance, cached int
					accept          bool
				}{{101, 90, false}, {90, 101, true}} {
					user := createCheckinTestUser(t, db, tc.balance)
					_, err := model.GetUserCache(user.Id)
					require.NoError(t, err)
					var key string
					for _, candidate := range server.Keys() {
						if server.HGet(candidate, "Id") == strconv.Itoa(user.Id) {
							key = candidate
							break
						}
					}
					require.NotEmpty(t, key)
					server.HSet(key, "Quota", strconv.Itoa(tc.cached))
					response := requestCheckinTest(t, user.Id)
					require.Equal(t, tc.accept, response.Success)
					cache, err := model.GetUserCache(user.Id)
					require.NoError(t, err)
					require.NoError(t, db.First(&user, user.Id).Error)
					if tc.accept {
						assert.Equal(t, tc.balance+25, user.Quota)
						assert.Equal(t, tc.cached+25, cache.Quota)
					} else {
						assert.Equal(t, exceeded, response.MessageKey)
						assert.Equal(t, tc.balance, user.Quota)
						assert.Equal(t, tc.cached, cache.Quota)
					}
				}
			})

			t.Run("first_option_insert_failure_does_not_publish", func(t *testing.T) {
				configureCheckinTest(t, true, nil)
				var count int64
				require.NoError(t, db.Model(&model.Option{}).Where(&model.Option{Key: optionKey}).Count(&count).Error)
				require.Zero(t, count)
				previousValue, previouslyPresent := common.OptionMap[optionKey]
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("checkin_option_insert_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "options" {
						tx.AddError(errors.New("injected first option insert failure"))
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove("checkin_option_insert_failure")) })
				var response checkinTestResponse
				modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: optionKey, Value: "100"}, &response)
				assert.False(t, response.Success)
				require.NoError(t, db.Model(&model.Option{}).Where(&model.Option{Key: optionKey}).Count(&count).Error)
				assert.Zero(t, count)
				currentValue, currentlyPresent := common.OptionMap[optionKey]
				assert.Equal(t, previouslyPresent, currentlyPresent)
				assert.Equal(t, previousValue, currentValue)
				assert.Nil(t, operation_setting.GetCheckinSetting().AllowedQuota)
			})

			t.Run("option_persistence_and_validation", func(t *testing.T) {
				configureCheckinTest(t, true, nil)
				common.OptionMapRWMutex.Lock()
				loadErr := config.GlobalConfig.LoadFromDB(map[string]string{"checkin_setting.enabled": "true"})
				common.OptionMapRWMutex.Unlock()
				require.NoError(t, loadErr)
				assert.Nil(t, operation_setting.GetCheckinSetting().AllowedQuota, "a legacy config without this option remains unrestricted")
				for _, raw := range []string{"0", "100", "null", strconv.FormatInt(common.MaxWalletQuota, 10)} {
					var response checkinTestResponse
					modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: optionKey, Value: raw}, &response)
					require.True(t, response.Success)
					var stored model.Option
					require.NoError(t, db.Where(&model.Option{Key: optionKey}).First(&stored).Error)
					assert.Equal(t, raw, stored.Value)
					var listed struct {
						Success bool
						Data    []model.Option
					}
					modelManagementRequest(t, GetOptions, http.MethodGet, "/api/option/", nil, &listed)
					require.True(t, listed.Success)
					assert.Contains(t, listed.Data, stored)
					for range 2 {
						// Reset the in-memory field before restoring the persisted representation.
						common.OptionMapRWMutex.Lock()
						resetErr := config.UpdateConfigFromMap(config.GlobalConfig.Get("checkin_setting"), map[string]string{"allowed_quota": "7"})
						loadErr := config.GlobalConfig.LoadFromDB(map[string]string{stored.Key: stored.Value})
						common.OptionMapRWMutex.Unlock()
						require.NoError(t, resetErr)
						require.NoError(t, loadErr)
						current := operation_setting.GetCheckinSetting().AllowedQuota
						if raw == "null" {
							assert.Nil(t, current)
						} else {
							expected, err := strconv.Atoi(raw)
							require.NoError(t, err)
							require.NotNil(t, current)
							assert.Equal(t, expected, *current)
						}
					}
				}
				require.NoError(t, model.UpdateOption(optionKey, "100"))
				for _, raw := range []string{"", "-1", "1.5", "1.0", "abc", "NaN", "Infinity", "true", "{}", "9007199254740992"} {
					var response checkinTestResponse
					modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: optionKey, Value: raw}, &response)
					assert.False(t, response.Success, raw)
					assert.Equal(t, invalid, response.MessageKey, raw)
					assert.Error(t, model.UpdateOptionsBulk(map[string]string{optionKey: raw}), raw)
				}
				var stored model.Option
				require.NoError(t, db.Where(&model.Option{Key: optionKey}).First(&stored).Error)
				assert.Equal(t, "100", stored.Value)
				require.NotNil(t, operation_setting.GetCheckinSetting().AllowedQuota)
				assert.Equal(t, 100, *operation_setting.GetCheckinSetting().AllowedQuota)
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("checkin_option_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "options" {
						tx.AddError(errors.New("injected option save failure"))
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("checkin_option_failure")) })
				var response checkinTestResponse
				modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: optionKey, Value: "null"}, &response)
				assert.False(t, response.Success)
				require.NoError(t, db.Where(&model.Option{Key: optionKey}).First(&stored).Error)
				assert.Equal(t, "100", stored.Value)
				assert.Equal(t, "100", common.OptionMap[optionKey])
				assert.Equal(t, 100, *operation_setting.GetCheckinSetting().AllowedQuota)
			})
		})
	}
}
