package model

import (
	"math/rand"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
)

// Checkin 签到记录
type Checkin struct {
	Id           int    `json:"id" gorm:"primaryKey;autoIncrement"`
	UserId       int    `json:"user_id" gorm:"not null;uniqueIndex:idx_user_checkin_date"`
	CheckinDate  string `json:"checkin_date" gorm:"type:varchar(10);not null;uniqueIndex:idx_user_checkin_date"` // 格式: YYYY-MM-DD
	QuotaAwarded int    `json:"quota_awarded" gorm:"not null"`
	CreatedAt    int64  `json:"created_at" gorm:"bigint"`
}

// CheckinRecord 用于API返回的签到记录（不包含敏感字段）
type CheckinRecord struct {
	CheckinDate  string `json:"checkin_date"`
	QuotaAwarded int    `json:"quota_awarded"`
}

func (Checkin) TableName() string {
	return "checkins"
}

// GetUserCheckinRecords 获取用户在指定日期范围内的签到记录
func GetUserCheckinRecords(userId int, startDate, endDate string) ([]Checkin, error) {
	var records []Checkin
	err := DB.Where("user_id = ? AND checkin_date >= ? AND checkin_date <= ?",
		userId, startDate, endDate).
		Order("checkin_date DESC").
		Find(&records).Error
	return records, err
}

// HasCheckedInToday 检查用户今天是否已签到
func HasCheckedInToday(userId int) (bool, error) {
	today := time.Now().Format("2006-01-02")
	var count int64
	err := DB.Model(&Checkin{}).
		Where("user_id = ? AND checkin_date = ?", userId, today).
		Count(&count).Error
	return count > 0, err
}

// UserCheckin checks eligibility and commits the daily record and wallet reward
// together. The balance comes from the database, like the dashboard profile.
func UserCheckin(userId int) (*Checkin, error) {
	setting := operation_setting.GetCheckinSetting()
	if !setting.Enabled {
		return nil, common.NewMessage("Check-in feature is not enabled")
	}

	now := time.Now()
	checkin := &Checkin{
		UserId:      userId,
		CheckinDate: now.Format("2006-01-02"),
		CreatedAt:   now.Unix(),
	}

	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Select("id", "quota").First(&user, userId).Error; err != nil {
			return err
		}
		// nil disables this check; zero remains an active eligibility limit.
		if setting.AllowedQuota != nil && user.Quota > *setting.AllowedQuota {
			return common.NewMessage("Your current balance exceeds the check-in eligibility quota. Please avoid repeated check-ins.")
		}

		var count int64
		if err := tx.Model(&Checkin{}).
			Where("user_id = ? AND checkin_date = ?", userId, checkin.CheckinDate).
			Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return common.NewMessage("Already checked in today")
		}

		// Bound the reward before random range arithmetic or wallet addition.
		if setting.MinQuota < 0 || setting.MinQuota > common.MaxWalletQuota ||
			setting.MaxQuota < 0 || setting.MaxQuota > common.MaxWalletQuota {
			return common.NewMessage("Check-in failed: quota update error")
		}
		checkin.QuotaAwarded = setting.MinQuota
		if setting.MaxQuota > setting.MinQuota {
			checkin.QuotaAwarded += rand.Intn(setting.MaxQuota - setting.MinQuota + 1)
		}
		if user.Quota > common.MaxWalletQuota-checkin.QuotaAwarded {
			return ErrWalletQuotaLimitExceeded
		}

		// The unique (user_id, checkin_date) index also guards concurrent requests.
		if err := tx.Create(checkin).Error; err != nil {
			return common.NewMessage("Check-in failed, please try again later")
		}

		// A zero reward still records a check-in. Avoid relying on MySQL's
		// affected-row count for an UPDATE that does not change the balance.
		if checkin.QuotaAwarded > 0 {
			result := tx.Model(&User{}).
				Where("id = ? AND quota <= ?", userId, common.MaxWalletQuota-checkin.QuotaAwarded).
				Update("quota", gorm.Expr("quota + ?", checkin.QuotaAwarded))
			if result.Error != nil || result.RowsAffected != 1 {
				return common.NewMessage("Check-in failed: quota update error")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	syncCreditUserQuotaCache(userId, checkin.QuotaAwarded, "check-in")
	return checkin, nil
}

// GetUserCheckinStats 获取用户签到统计信息
func GetUserCheckinStats(userId int, month string) (map[string]any, error) {
	// 获取指定月份的所有签到记录
	startDate := month + "-01"
	endDate := month + "-31"

	records, err := GetUserCheckinRecords(userId, startDate, endDate)
	if err != nil {
		return nil, err
	}

	// 转换为不包含敏感字段的记录
	checkinRecords := make([]CheckinRecord, len(records))
	for i, r := range records {
		checkinRecords[i] = CheckinRecord{
			CheckinDate:  r.CheckinDate,
			QuotaAwarded: r.QuotaAwarded,
		}
	}

	// 检查今天是否已签到
	hasCheckedToday, _ := HasCheckedInToday(userId)

	// 获取用户所有时间的签到统计
	var totalCheckins int64
	var totalQuota int64
	DB.Model(&Checkin{}).Where("user_id = ?", userId).Count(&totalCheckins)
	DB.Model(&Checkin{}).Where("user_id = ?", userId).Select("COALESCE(SUM(quota_awarded), 0)").Scan(&totalQuota)

	return map[string]any{
		"total_quota":      totalQuota,      // 所有时间累计获得的额度
		"total_checkins":   totalCheckins,   // 所有时间累计签到次数
		"checkin_count":    len(records),    // 本月签到次数
		"checked_in_today": hasCheckedToday, // 今天是否已签到
		"records":          checkinRecords,  // 本月签到记录详情（不含id和user_id）
	}, nil
}
