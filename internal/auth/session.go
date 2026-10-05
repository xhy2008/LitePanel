package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync/atomic"
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
	// ttlNanos 用原子而不是裸字段：设置页会在运行中改它（SetTTL），而读它
	// 的 Validate 在每个受保护请求上被并发调用。裸字段是数据竞争，而本机
	// -race 不可用（Termux 跑不了），只能靠结构保证。
	ttlNanos atomic.Int64
}

func NewSessionStore(db *store.DB, now func() time.Time, ttl time.Duration) *SessionStore {
	if now == nil {
		now = time.Now
	}
	ss := &SessionStore{db: db, now: now}
	ss.ttlNanos.Store(int64(ttl))
	return ss
}

// TTL 返回当前会话有效期。
func (s *SessionStore) TTL() time.Duration {
	d := time.Duration(s.ttlNanos.Load())
	if d <= 0 {
		// 0/负值 = 库里留下的坏数字。回落到一个明确的安全默认而不是"永不过期"
		// 或"立即过期"：前者让会话有效期这项设置变成反向的安全漏洞，后者
		// 会把用户当场踢下线并让他以为自己密码错了。
		return 30 * 24 * time.Hour
	}
	return d
}

// SetTTL 热改会话有效期（设置页）。返回因"有效期变短"而被收紧的既有会话数。
//
// 为什么必须动库里的老会话：Validate 每次校验都按**当前** ttl 滑动续期，
// 所以正在被使用的会话下一次请求就收敛到新值 —— 但**没在被使用**的会话
// 仍按它落库时的 expires_at 判定。用户把有效期从 30 天调到 1 天的动机往往
// 正是"怀疑别人拿着我的登录态"，这时"只有对方在线时才生效"等于没生效。
//
// 只收紧、不延长：把有效期从 1 天调到 30 天不该让既有会话凭空多活 ——
// 那等于"改大设置会把已经过期的会话复活"之类的荒谬行为，而且用户无法
// 通过它拿到任何他本来拿不到的东西（他自己重新登录就行）。
func (s *SessionStore) SetTTL(ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, fmt.Errorf("会话有效期必须为正，当前 %v", ttl)
	}
	s.ttlNanos.Store(int64(ttl))
	newExpiry := s.now().Add(ttl).Unix()
	// 两个条件缺一个都不对：
	//   expires_at > now        —— 已过期的行不动（它们下一次被校验时会被
	//                             Validate 删掉，不该在这里被“救活”）；
	//   expires_at > newExpiry  —— 只**收紧**。少了这一条，把有效期从 1 天
	//                             调到 30 天会把所有既有会话的到期时间往后
	//                             推，等于“改大设置就能把已经踢掉的登录态
	//                             拉回来”——那是提权而不是调参数。
	res, err := s.db.SqlDB().Exec(
		`UPDATE sessions SET expires_at=? WHERE expires_at > ? AND expires_at > ?`,
		newExpiry, s.now().Unix(), newExpiry)
	if err != nil {
		return 0, fmt.Errorf("收紧既有会话的过期时间: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
		hashToken(token), now.Unix(), now.Add(s.TTL()).Unix(), ua, ip, now.Unix(),
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
	newExpiry := now.Add(s.TTL())
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
