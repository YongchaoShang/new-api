package operation_setting

import (
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

const (
	LogMaxRangeDaysOptionKey      = "log_setting.max_range_days"
	LogRedactionKeywordsOptionKey = "log_setting.redaction_keywords"
)

// LogSetting controls usage-log access and shared error redaction.
type LogSetting struct {
	// UserExportEnabled lets regular users export their own usage logs.
	// Administrators can always export.
	UserExportEnabled bool `json:"user_export_enabled"`
	// MaxRangeDays is the longest time range a user can search in their logs
	// and dashboard, and the longest range any export can cover.
	MaxRangeDays int `json:"max_range_days"`
	// RedactionKeywords extend the existing upstream-information masks.
	RedactionKeywords []string `json:"redaction_keywords"`
}

var logSetting = LogSetting{
	UserExportEnabled: true,
	MaxRangeDays:      365,
	RedactionKeywords: []string{},
}

func init() {
	config.GlobalConfig.Register("log_setting", &logSetting)
}

func GetLogSetting() *LogSetting {
	return &logSetting
}

func (s *LogSetting) MaxRangeSeconds() int64 {
	return int64(max(s.MaxRangeDays, 1)) * 24 * 60 * 60
}

func ValidateLogMaxRangeDays(value string) error {
	days, err := strconv.Atoi(value)
	if err != nil || days < 1 {
		return common.NewMessage("The longest log time range must be at least 1 day")
	}
	return nil
}

// ParseLogRedactionKeywords keeps input order for equal-length replacements.
// Decode entries separately from their type check so JSON null cannot silently
// become an empty string inside an otherwise valid array.
func ParseLogRedactionKeywords(value string) ([]string, error) {
	var entries []any
	if err := common.UnmarshalJsonStr(value, &entries); err != nil || entries == nil {
		return nil, common.NewMessage("Redaction keywords must be a JSON array of strings")
	}
	keywords := make([]string, 0, len(entries))
	for _, entry := range entries {
		keyword, ok := entry.(string)
		if !ok {
			return nil, common.NewMessage("Redaction keywords must be a JSON array of strings")
		}
		keyword = strings.TrimSpace(keyword)
		if keyword == "" || slices.ContainsFunc(keywords, func(existing string) bool {
			return strings.EqualFold(existing, keyword)
		}) {
			continue
		}
		keywords = append(keywords, keyword)
	}
	return keywords, nil
}
