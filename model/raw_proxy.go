package model

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

// RawProxyRequest is bookkeeping only: upstream bodies and credentials are never
// persisted. KeyHash locates the original key even after reordering, without persisting it.
type RawProxyRequest struct {
	ID              string  `gorm:"size:36;primaryKey"`
	PluginKey       string  `gorm:"size:30;not null;uniqueIndex:uk_raw_upstream,priority:1"`
	ChannelID       int     `gorm:"not null;uniqueIndex:uk_raw_upstream,priority:2"`
	UpstreamID      *string `gorm:"size:128;uniqueIndex:uk_raw_upstream,priority:3"`
	UserID          int     `gorm:"not null;index"`
	TokenID         int     `gorm:"not null"`
	TokenName       string  `gorm:"size:255"`
	Model           string  `gorm:"size:255;not null"`
	Group           string  `gorm:"size:64"`
	KeyIndex        int
	KeyHash         string `gorm:"size:64"`
	CreatedAt       int64
	Billed          bool `gorm:"not null"`
	CostUSD         float64
	Quota           int
	AccountingError string `gorm:"size:255"`
}

func FindRawProxyRequest(plugin, upstreamID string, userID int) (*RawProxyRequest, error) {
	var rows []RawProxyRequest
	err := DB.Where("plugin_key = ? AND upstream_id = ? AND user_id = ?", plugin, upstreamID, userID).Limit(2).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, gorm.ErrRecordNotFound
	}
	return &rows[0], nil
}

// RawProxyChannel selects the highest priority eligible channel using the
// existing weight semantics. Raw does not consult the model catalogue.
func RawProxyChannel(plugin, group string, pinnedID int) (*Channel, error) {
	var candidates []Channel
	query := DB.Where("status = ? AND type = ?", common.ChannelStatusEnabled, constant.ChannelTypeTaskPlugin)
	if pinnedID != 0 {
		query = query.Where("id = ?", pinnedID)
	}
	if err := query.Order("priority desc").Find(&candidates).Error; err != nil {
		return nil, err
	}
	eligible := make([]Channel, 0, len(candidates))
	for _, channel := range candidates {
		settings := channel.GetSetting()
		if !settings.RawProxyEnabled || !settings.BindsTaskPlugin(plugin) || !slices.Contains(channel.GetGroups(), group) {
			continue
		}
		if len(eligible) > 0 && channel.GetPriority() < eligible[0].GetPriority() {
			break
		}
		eligible = append(eligible, channel)
	}
	if len(eligible) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	total := 0
	for _, channel := range eligible {
		total += channel.GetWeight() + 10
	}
	selected := common.GetRandomInt(total)
	for i := range eligible {
		selected -= eligible[i].GetWeight() + 10
		if selected < 0 {
			return &eligible[i], nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func CreateRawProxyRequest(record *RawProxyRequest) error { return DB.Create(record).Error }

func BindRawProxyRequest(record *RawProxyRequest, upstreamID string) error {
	if upstreamID == "" || len(upstreamID) > 128 || strings.ContainsAny(upstreamID, "/\\\x00\r\n") {
		return errors.New("invalid upstream request ID")
	}
	result := DB.Model(&RawProxyRequest{}).Where("id = ? AND upstream_id IS NULL", record.ID).Update("upstream_id", upstreamID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		record.UpstreamID = &upstreamID
		return nil
	}
	return errors.New("upstream request ID already bound")
}

func RawProxyAccountingError(id, reason string) {
	// Only host-owned reason codes are stored; never hook/provider text.
	DB.Model(&RawProxyRequest{}).Where("id = ? AND billed = ?", id, false).Update("accounting_error", reason)
}

// SettleRawProxyRequest commits the dedup marker and every quota delta together.
// No batch queue is involved: a restart cannot lose the charge after the marker.
func SettleRawProxyRequest(record *RawProxyRequest, usd float64, quota int) (bool, error) {
	if quota < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return false, errors.New("negative raw quota")
	}
	charged := false
	var token Token
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&RawProxyRequest{}).Where("id = ? AND billed = ?", record.ID, false).Updates(map[string]any{"billed": true, "quota": quota, "cost_usd": usd, "accounting_error": ""})
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		if err := tx.Unscoped().First(&token, record.TokenID).Error; err != nil {
			return err
		}
		if token.UserId != record.UserID {
			return errors.New("raw token ownership mismatch")
		}
		user := tx.Model(&User{}).Where("id = ?", record.UserID).Updates(map[string]any{"quota": gorm.Expr("quota - ?", quota), "used_quota": gorm.Expr("used_quota + ?", quota), "request_count": gorm.Expr("request_count + 1")})
		if user.Error != nil {
			return user.Error
		}
		if user.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		result = tx.Unscoped().Model(&Token{}).Where("id = ?", record.TokenID).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", quota), "used_quota": gorm.Expr("used_quota + ?", quota), "accessed_time": common.GetTimestamp()})
		if result.Error != nil {
			return result.Error
		}
		result = tx.Model(&Channel{}).Where("id = ?", record.ChannelID).Update("used_quota", gorm.Expr("used_quota + ?", quota))
		if result.Error != nil {
			return result.Error
		}
		// MySQL reports changed rows: a legitimate zero-cost update is zero.
		if result.RowsAffected != 1 {
			var count int64
			if err := tx.Model(&Channel{}).Where("id = ?", record.ChannelID).Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return gorm.ErrRecordNotFound
			}
		}
		charged = true
		return nil
	})
	if err != nil || !charged {
		return false, err
	}
	record.Billed, record.CostUSD, record.Quota = true, usd, quota
	if common.RedisEnabled {
		if _, err := cacheApplyUserQuotaDelta(record.UserID, -int64(quota)); err != nil {
			common.SysError(fmt.Sprintf("raw user quota cache update failed user=%d", record.UserID))
		}
		if _, err := cacheApplyTokenQuotaDelta(record.TokenID, token.Key, -int64(quota)); err != nil {
			common.SysError(fmt.Sprintf("raw token quota cache update failed token=%d", record.TokenID))
		}
	}
	return true, nil
}
