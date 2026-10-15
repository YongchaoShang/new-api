package operation_setting

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

const CheckinAllowedQuotaOptionKey = "checkin_setting.allowed_quota"

// CheckinSetting 签到功能配置
type CheckinSetting struct {
	Enabled      bool `json:"enabled"`       // 是否启用签到功能
	MinQuota     int  `json:"min_quota"`     // 签到最小额度奖励
	MaxQuota     int  `json:"max_quota"`     // 签到最大额度奖励
	AllowedQuota *int `json:"allowed_quota"` // nil disables the balance limit; zero is an active limit.
}

// 默认配置
var checkinSetting = CheckinSetting{
	Enabled:  false, // 默认关闭
	MinQuota: 1000,  // 默认最小额度 1000 (约 0.002 USD)
	MaxQuota: 10000, // 默认最大额度 10000 (约 0.02 USD)
}

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("checkin_setting", &checkinSetting)
}

// GetCheckinSetting returns a snapshot, including a copy of the optional limit.
// Option updates use OptionMapRWMutex while updating the registered config.
func GetCheckinSetting() *CheckinSetting {
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()
	setting := checkinSetting
	if setting.AllowedQuota != nil {
		quota := *setting.AllowedQuota
		setting.AllowedQuota = &quota
	}
	return &setting
}

// IsCheckinEnabled 是否启用签到功能
func IsCheckinEnabled() bool {
	return GetCheckinSetting().Enabled
}

// GetCheckinQuotaRange 获取签到额度范围
func GetCheckinQuotaRange() (min, max int) {
	setting := GetCheckinSetting()
	return setting.MinQuota, setting.MaxQuota
}

// ValidateCheckinAllowedQuota accepts the option's JSON integer or null encoding.
func ValidateCheckinAllowedQuota(value string) error {
	var quota *int
	if err := common.UnmarshalJsonStr(value, &quota); err != nil ||
		(quota != nil && (*quota < 0 || *quota > common.MaxWalletQuota)) {
		return common.NewMessage("Check-in eligibility quota must be empty or a non-negative safe integer")
	}
	return nil
}
