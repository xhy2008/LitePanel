package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"litepanel/internal/store"
)

// Session 是一条登录会话（数据库行的视图）。
type Session struct {
	TokenHash string
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
	LastIP    string
	LastSeen  time.Time
}

// SessionStore 签发并校验会话 token。
// token 本体只发给浏览器，库里存它的 SHA-256（设计 5.7）。
type SessionStore struct {
	db  *store.DB
	now func() time.Time
	ttl time.Duration
}

func NewSessionStore(db *store.DB, now func() time.Time, ttl time.Duration) *SessionStore {
	if now == nil {
		now = time.Now
	}
	return &SessionStore{db: db, now: now, ttl: ttl}
}

// Issue 生成 32 字节 CSPRNG token，落库存哈希，返回明文（仅此一次）。
func (s *SessionStore) Issue(ua, ip string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("生成会话 token 失败: " + err.Error())
	}
	token := hex.EncodeToString(raw)
	now := s.now()
	_, err := s.db.SqlDB().Exec(
		`INSERT INTO sessions(token_hash,created_at,expires_at,user_agent,last_ip,last_seen)
		 VALUES(?,?,?,?,?,?)`,
		hashToken(token), now.Unix(), now.Add(s.ttl).Unix(), ua, ip, now.Unix(),
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// Validate 校验 token；有效时滑动续期并返回会话行。
func (s *SessionStore) Validate(token string) (*Session, bool, error) {
	if token == "" {
		return nil, false, nil
	}
	h := hashToken(token)
	var (
		createdAt, expiresAt, lastSeen int64
		ua, ip                         sql.NullString
	)
	err := s.db.SqlDB().QueryRow(
		`SELECT created_at,expires_at,user_agent,last_ip,last_seen FROM sessions WHERE token_hash=?`,
		h,
	).Scan(&createdAt, &expiresAt, &ua, &ip, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	now := s.now()
	if now.Unix() >= expiresAt {
		// 已过期：顺手删掉，别等定时清理。
		_, _ = s.db.SqlDB().Exec(`DELETE FROM sessions WHERE token_hash=?`, h)
		return nil, false, nil
	}
	newExpiry := now.Add(s.ttl)
	if _, err := s.db.SqlDB().Exec(
		`UPDATE sessions SET expires_at=?, last_seen=? WHERE token_hash=?`,
		newExpiry.Unix(), now.Unix(), h,
	); err != nil {
		return nil, false, err
	}
	return &Session{
		TokenHash: h,
		CreatedAt: time.Unix(createdAt, 0),
		ExpiresAt: newExpiry,
		UserAgent: ua.String,
		LastIP:    ip.String,
		LastSeen:  time.Unix(lastSeen, 0),
	}, true, nil
}

// Revoke 吊销单个 token（登出当前设备）。
func (s *SessionStore) Revoke(token string) error {
	_, err := s.db.SqlDB().Exec(`DELETE FROM sessions WHERE token_hash=?`, hashToken(token))
	return err
}

// RevokeAll 吊销全部会话（改密后强制失效，设计中明确要求）。
func (s *SessionStore) RevokeAll() error {
	_, err := s.db.SqlDB().Exec(`DELETE FROM sessions`)
	return err
}

// Cleanup 删除所有已过期的会话，返回删除条数。
func (s *SessionStore) Cleanup() (int, error) {
	res, err := s.db.SqlDB().Exec(`DELETE FROM sessions WHERE expires_at < ?`, s.now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
