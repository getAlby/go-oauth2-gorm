package oauth2gorm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-oauth2/oauth2/v4"
	"github.com/go-oauth2/oauth2/v4/models"
	"gorm.io/gorm"
)

type TokenStoreItem struct {
	gorm.Model

	ExpiresAt   time.Time
	Code        string `gorm:"type:varchar(512)"`
	Access      string `gorm:"type:varchar(512)"`
	Refresh     string `gorm:"type:varchar(512)"`
	ClientID    string `gorm:"type:varchar(512)"`
	UserID      string `gorm:"type:varchar(512)"`
	Scope       string `gorm:"type:varchar(512)"`
	RedirectURI string `gorm:"type:varchar(512)"`
	Data        string `gorm:"type:text"`
}

// NewStore create mysql store instance,
func NewTokenStore(config *Config, gcInterval int) *TokenStore {

	db, err := gorm.Open(config.Dialector, defaultConfig)
	if err != nil {
		panic(err)
	}
	// default client pool
	s, err := db.DB()
	if err != nil {
		panic(err)
	}
	s.SetMaxIdleConns(10)
	s.SetMaxOpenConns(100)
	s.SetConnMaxLifetime(time.Hour)

	return NewTokenStoreWithDB(config, db, gcInterval)
}

func NewTokenStoreWithDB(config *Config, db *gorm.DB, gcInterval int) *TokenStore {
	store := &TokenStore{
		db:        db,
		tableName: "oauth2_token",
		stdout:    os.Stderr,
	}
	if config.TableName != "" {
		store.tableName = config.TableName
	}
	interval := 600
	if gcInterval > 0 {
		interval = gcInterval
	}
	store.ticker = time.NewTicker(time.Second * time.Duration(interval))

	if !db.Migrator().HasTable(store.tableName) {
		if err := db.Table(store.tableName).Migrator().CreateTable(&TokenStoreItem{}); err != nil {
			panic(err)
		}
	}

	go store.gc()
	return store
}

// Store mysql token store
type TokenStore struct {
	tableName string
	db        *gorm.DB
	stdout    io.Writer
	ticker    *time.Ticker
}

// SetStdout set error output
func (s *TokenStore) SetStdout(stdout io.Writer) *TokenStore {
	s.stdout = stdout
	return s
}

// Close close the store
func (s *TokenStore) Close() {
	s.ticker.Stop()
}

func (s *TokenStore) errorf(format string, args ...interface{}) {
	if s.stdout != nil {
		buf := fmt.Sprintf(format, args...)
		s.stdout.Write([]byte(buf))
	}
}

func (s *TokenStore) gc() {
	for range s.ticker.C {
		now := time.Now()
		var count int64
		if err := s.db.Table(s.tableName).Where("expires_at <= ?", now).Or("code = ? and access = ? AND refresh = ?", "", "", "").Count(&count).Error; err != nil {
			s.errorf("[ERROR]:%s\n", err)
			return
		}
		if count > 0 {
			// not soft delete.
			if err := s.db.Table(s.tableName).Where("expires_at <= ?", now).Or("code = ? and access = ? AND refresh = ?", "", "", "").Unscoped().Delete(&TokenStoreItem{}).Error; err != nil {
				s.errorf("[ERROR]:%s\n", err)
			}
		}
	}
}

// Create create and store the new token information
func (s *TokenStore) Create(ctx context.Context, info oauth2.TokenInfo) error {
	item := &TokenStoreItem{
		ClientID:    info.GetClientID(),
		UserID:      info.GetUserID(),
		Scope:       info.GetScope(),
		RedirectURI: info.GetRedirectURI(),
	}

	if code := info.GetCode(); code != "" {
		item.Code = code
		item.ExpiresAt = info.GetCodeCreateAt().Add(info.GetCodeExpiresIn())
	} else {
		item.Access = info.GetAccess()
		accessExpiry := info.GetAccessCreateAt().Add(info.GetAccessExpiresIn())
		var refreshExpiry time.Time
		if refresh := info.GetRefresh(); refresh != "" {
			item.Refresh = info.GetRefresh()
			refreshExpiry = info.GetRefreshCreateAt().Add(info.GetRefreshExpiresIn())
		}
		if (accessExpiry.After(refreshExpiry)) {
			item.ExpiresAt = accessExpiry
			info.SetRefreshExpiresIn(info.GetAccessExpiresIn())
		} else {
			item.ExpiresAt = refreshExpiry
		}
	}

	jv, err := json.Marshal(info)
	if err != nil {
		return err
	}
	item.Data = string(jv)

	return s.db.WithContext(ctx).Table(s.tableName).Create(item).Error
}

// RemoveByCode delete the authorization code
func (s *TokenStore) RemoveByCode(ctx context.Context, code string) error {
	return s.db.WithContext(ctx).
		Table(s.tableName).
		Where("code = ?", code).
		Update("code", "").
		Error
}

// RemoveByAccess use the access token to delete the token information
func (s *TokenStore) RemoveByAccess(ctx context.Context, access string) error {
	return s.db.WithContext(ctx).
		Table(s.tableName).
		Where("access = ?", access).
		Update("access", "").
		Error
}

// RemoveByRefresh use the refresh token to delete the token information
func (s *TokenStore) RemoveByRefresh(ctx context.Context, refresh string) error {
	return s.db.WithContext(ctx).
		Table(s.tableName).
		Where("refresh = ?", refresh).
		Update("refresh", "").
		Error
}

func (s *TokenStore) toTokenInfo(data string) oauth2.TokenInfo {
	var tm models.Token
	err := json.Unmarshal([]byte(data), &tm)
	if err != nil {
		return nil
	}
	return &tm
}

// GetByCode use the authorization code for token information data
func (s *TokenStore) GetByCode(ctx context.Context, code string) (oauth2.TokenInfo, error) {
	if code == "" {
		return nil, nil
	}

	var item TokenStoreItem
	if err := s.db.WithContext(ctx).
		Table(s.tableName).
		Where("code = ?", code).
		Find(&item).Error; err != nil {
		return nil, err
	}
	if item.ID == 0 {
		return nil, nil
	}

	return s.toTokenInfo(item.Data), nil
}

// GetByAccess use the access token for token information data
func (s *TokenStore) GetByAccess(ctx context.Context, access string) (oauth2.TokenInfo, error) {
	if access == "" {
		return nil, nil
	}

	var item TokenStoreItem
	if err := s.db.WithContext(ctx).
		Table(s.tableName).
		Where("access = ?", access).
		First(&item).Error; err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	if item.ID == 0 {
		return nil, nil
	}

	return s.toTokenInfo(item.Data), nil
}

// GetByRefresh use the refresh token for token information data
func (s *TokenStore) GetByRefresh(ctx context.Context, refresh string) (oauth2.TokenInfo, error) {
	if refresh == "" {
		return nil, nil
	}

	var item TokenStoreItem
	if err := s.db.WithContext(ctx).
		Table(s.tableName).
		Where("refresh = ?", refresh).
		Find(&item).Error; err != nil {
		return nil, err
	}
	if item.ID == 0 {
		return nil, nil
	}

	return s.toTokenInfo(item.Data), nil
}
