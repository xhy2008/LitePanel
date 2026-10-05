package settings

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"litepanel/internal/store"
)

func newStore(t *testing.T, cfgDefault func(Key) string) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, cfgDefault, func() time.Time { return time.Unix(1700000000, 0) })
}

// put 构造一批设置写入（JSON 数字/字符串混用）。
func put(t *testing.T, s *Store, kv map[string]any) []Value {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for k, v := range kv {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		raw[k] = b
	}
	out, err := s.Apply(context.Background(), raw)
	if err != nil {
		t.Fatalf("Apply(%v): %v", kv, err)
	}
	return out
}

func find(t *testing.T, vs []Value, k Key) Value {
	t.Helper()
	for _, v := range vs {
		if v.Key == k {
			return v
		}
	}
	t.Fatalf("没有返回 %s", k)
	return Value{}
}

// 全量返回必须包含注册表里的每一项，且带分组/标签等展示元数据。
// 缺一项的形态：设置页少一个分组，用户以为功能没做。
func TestAllReturnsEveryKnownKey(t *testing.T) {
	s := newStore(t, nil)
	all, err := s.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(registry) {
		t.Fatalf("返回 %d 项，注册表有 %d 项", len(all), len(registry))
	}
	for _, v := range all {
		if v.Label == "" || v.Group == "" {
			t.Errorf("%s 缺 Label/Group，前端分组导航会漏掉它", v.Key)
		}
	}
}

// 库里没写过时回内置默认，且 Overridden=false（前端要显示"默认"标记）。
func TestDefaultsWhenNothingStored(t *testing.T) {
	s := newStore(t, nil)
	all, _ := s.All(context.Background())
	v := find(t, all, MetricIntervalSec)
	if v.Value != "1" {
		t.Errorf("采样间隔默认应为 1，得 %q", v.Value)
	}
	if v.Overridden {
		t.Error("没写过不该算已覆盖")
	}
	if v.Int() != 1 {
		t.Errorf("Int() = %d", v.Int())
	}
}

// 装配层可以从 config 兜底（配置文件是部署时生成的，设置页是事后微调）。
// 优先级：库里 > config > 内置默认。优先级反了的后果：用户在设置页改了
// 采样间隔，下次重启被配置文件盖回去。
func TestConfigFallbackOrdering(t *testing.T) {
	s := newStore(t, func(k Key) string {
		if k == Aria2RPCURL {
			return "http://127.0.0.1:6800/jsonrpc"
		}
		if k == TrashRetainDays {
			return "7"
		}
		return ""
	})
	all, _ := s.All(context.Background())
	if got := find(t, all, Aria2RPCURL).Value; got != "http://127.0.0.1:6800/jsonrpc" {
		t.Errorf("aria2 地址没有从 config 兜底: %q", got)
	}
	if got := find(t, all, TrashRetainDays).Int(); got != 7 {
		t.Errorf("保留天数应从 config 兜底成 7，得 %d", got)
	}
	// 写库之后 config 让位。
	put(t, s, map[string]any{string(Aria2RPCURL): "http://localhost:6801/jsonrpc"})
	all, _ = s.All(context.Background())
	v := find(t, all, Aria2RPCURL)
	if v.Value != "http://localhost:6801/jsonrpc" || !v.Overridden {
		t.Errorf("库里的值应盖过 config: %+v", v)
	}
}

// 未知键必须报错而不是静默忽略。
// 忽略的后果："界面显示已保存、实际什么都没生效" —— 只存不读的设置项
// 最难发现的一种失效，而且这次是前端自己写错了 key，更没人会怀疑。
func TestApplyRejectsUnknownKey(t *testing.T) {
	s := newStore(t, nil)
	raw := map[string]json.RawMessage{
		"metric_interval_sec": json.RawMessage("2"),
		"made_up_key":         json.RawMessage(`"x"`),
	}
	_, err := s.Apply(context.Background(), raw)
	if err == nil {
		t.Fatal("未知键必须报错")
	}
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("错误类型 %v", err)
	}
	if !strings.Contains(err.Error(), "made_up_key") {
		t.Errorf("错误里要点出是哪个键: %v", err)
	}
	// 同一批里的合法键也不能写进去（整批原子）。
	all, _ := s.All(context.Background())
	if v := find(t, all, MetricIntervalSec); v.Overridden {
		t.Error("一批里有非法项时整批都不该写入")
	}
}

// 范围校验：越界的数字必须拒。
// 为什么边界值得报错而不是夹取：设置页是表单，用户填 100 天可能是真想
// 要 100 天，静默夹到 90 会让他以为已经生效。
func TestIntRangeValidation(t *testing.T) {
	s := newStore(t, nil)
	cases := []struct {
		key  Key
		val  any
		ok   bool
		want string
	}{
		{TrashRetainDays, 0, false, ""},
		{TrashRetainDays, 91, false, ""},
		{TrashRetainDays, -5, false, ""},
		{TrashRetainDays, 1, true, "1"},
		{TrashRetainDays, 90, true, "90"},
		{JobConcurrencyFile, 0, false, ""}, // 0 个 worker 是无声瘫痪，必须挡
		{JobConcurrencyFile, 17, false, ""},
		{ServiceLogLines, 10, false, ""},
		{StopGraceSec, 30, true, "30"},
	}
	for _, c := range cases {
		_, err := s.Apply(context.Background(), map[string]json.RawMessage{
			string(c.key): mustJSON(t, c.val),
		})
		if c.ok && err != nil {
			t.Errorf("%s=%v 应当合法: %v", c.key, c.val, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s=%v 应当被拒", c.key, c.val)
		}
	}
}

// 枚举项只能取列出的档位（设计：采样间隔 1/2/5s，回滚 5000/20000/100000）。
// 3 秒这种值被接受的后果：它不在前端下拉里，用户改完看不到选中态。
func TestEnumRejectsOffListValue(t *testing.T) {
	s := newStore(t, nil)
	for _, v := range []any{0, 3, 4, 10} {
		if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
			string(MetricIntervalSec): mustJSON(t, v),
		}); err == nil {
			t.Errorf("采样间隔 %v 不在 {1,2,5} 里，应被拒", v)
		}
	}
	for _, v := range []any{1, 2, 5} {
		if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
			string(MetricIntervalSec): mustJSON(t, v),
		}); err != nil {
			t.Errorf("采样间隔 %v 应合法: %v", v, err)
		}
	}
	// 表单常发字符串形式的数字，同样接受（拒它没有安全收益，只有摩擦）。
	if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
		string(MetricIntervalSec): json.RawMessage(`"2"`),
	}); err != nil {
		t.Errorf(`"2" 这种字符串数字应接受: %v`, err)
	}
	all, _ := s.All(context.Background())
	if got := find(t, all, MetricIntervalSec).Int(); got != 2 {
		t.Errorf("规范化后应是 2，得 %d", got)
	}
}

// 非数字发进数字项必须报错，且错误里带中文标签（用户看得懂是哪一项）。
func TestWrongTypeRejectedWithLabel(t *testing.T) {
	s := newStore(t, nil)
	_, err := s.Apply(context.Background(), map[string]json.RawMessage{
		string(TrashRetainDays): json.RawMessage(`"abc"`),
	})
	if err == nil {
		t.Fatal("非数字应被拒")
	}
	if !strings.Contains(err.Error(), "回收站保留天数") {
		t.Errorf("错误要指出是哪一项: %v", err)
	}
}

// 回收站目录名必须是单个路径段（与 config 的规则同源）。
// 写 "/DISK/.trash" 的后果：拼成 <盘根>/DISK/.trash，在每个盘上悄悄建目录。
func TestTrashDirNameValidation(t *testing.T) {
	s := newStore(t, nil)
	bad := []string{"/abs/.trash", "a/b", "..", "."}
	for _, v := range bad {
		if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
			string(TrashDirName): mustJSON(t, v),
		}); err == nil {
			t.Errorf("回收站名 %q 应被拒", v)
		}
	}
	// 空串也拒：那会让拼接变成 <盘根>/，等于把整盘当回收站。
	if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
		string(TrashDirName): json.RawMessage(`""`),
	}); err == nil {
		t.Error("空回收站名应被拒")
	}
	if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
		string(TrashDirName): json.RawMessage(`".trash"`),
	}); err != nil {
		t.Errorf(".trash 应合法: %v", err)
	}
}

// aria2 RPC 地址：明文 http 连非回环必须拒（与 config 门禁同源）。
// 库里能写进一个 config 会拒绝的值 = 设置页能绕过启动门禁，那这条门禁
// 等于没有（重启才生效的门禁也是门禁）。
func TestRPCURLPlaintextRemoteRejected(t *testing.T) {
	s := newStore(t, nil)
	bad := []string{"http://10.0.0.5:6800/jsonrpc", "http://evil.example.com/jsonrpc",
		"ftp://127.0.0.1:6800", "not a url", "http://127.0.0.1.evil.com:6800/jsonrpc"}
	for _, v := range bad {
		if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
			string(Aria2RPCURL): mustJSON(t, v),
		}); err == nil {
			t.Errorf("RPC 地址 %q 应被拒", v)
		}
	}
	ok := []string{"http://127.0.0.1:6800/jsonrpc", "http://localhost:6800/jsonrpc",
		"https://remote.example.com:6800/jsonrpc", "http://[::1]:6800/jsonrpc"}
	for _, v := range ok {
		if _, err := s.Apply(context.Background(), map[string]json.RawMessage{
			string(Aria2RPCURL): mustJSON(t, v),
		}); err != nil {
			t.Errorf("RPC 地址 %q 应合法: %v", v, err)
		}
	}
}

// 密钥项必须打 Secret 标记：GET 响应里不能原样回显密钥。
// 理由不是"密钥可能泄露给别的用户"（面板单用户），而是它会被写进浏览器
// 历史、被前端调试日志打印、被 Vue devtools 显示 —— 每一项都不是用户
// 期望的"密钥只存在服务器上"。
func TestSecretKeyIsMarked(t *testing.T) {
	s := newStore(t, nil)
	put(t, s, map[string]any{string(Aria2RPCSecret): "s3cr3t"})
	all, _ := s.All(context.Background())
	v := find(t, all, Aria2RPCSecret)
	if !v.Secret {
		t.Fatal("RPC 密钥必须带 Secret 标记，否则 API 层会原样回显")
	}
	if v.Value != "s3cr3t" {
		t.Errorf("存储层本身仍应能读到真值（装配层要用）: %q", v.Value)
	}
}

// 字符串项的空白会被规范化；默认下载目录这类可选项允许留空。
func TestStringTrimAndEmptyOptional(t *testing.T) {
	s := newStore(t, nil)
	put(t, s, map[string]any{string(Aria2DownloadDir): "  /data/downloads  "})
	all, _ := s.All(context.Background())
	if got := find(t, all, Aria2DownloadDir).Value; got != "/data/downloads" {
		t.Errorf("应去掉首尾空格: %q", got)
	}
	// 写过的项算已设置。
	if !find(t, all, Aria2DownloadDir).Set || !find(t, all, Aria2DownloadDir).Overridden {
		t.Error("写过的可选项应算已设置/已覆盖")
	}
	// 没写过的可选字符串项：Set=false（前端要显示"未设置"，而不是一个
	// 看起来像已保存了空值的空输入框）。
	fresh := newStore(t, nil)
	fall, _ := fresh.All(context.Background())
	if v := find(t, fall, Aria2DownloadDir); v.Set || v.Overridden || v.Value != "" {
		t.Errorf("没写过的可选项应是空且未设置: %+v", v)
	}
}

// 坏值（旧版本写入的、或规则收紧前留下的）在快照读取时回默认而不是报错。
// 让面板因为库里一个数字坏了而起不来，用户连改回来的界面都打不开。
func TestSnapshotToleratesCorruptValue(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SqlDB().Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES('metric_interval_sec','oops',0)`); err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, nil, nil)
	// 再放一个正常的字符串项，验证快照能读到库里的值（不只是回默认）。
	if _, err := db.SqlDB().Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES('aria2_rpc_url','http://127.0.0.1:6800/jsonrpc',0)`); err != nil {
		t.Fatal(err)
	}
	sn, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := sn.Int(MetricIntervalSec, DefaultMetricIntervalSec); got != DefaultMetricIntervalSec {
		t.Errorf("坏值应回默认，得 %d", got)
	}
	if got := sn.Int(SessionTTLDays, 30); got != 30 {
		t.Errorf("缺失应回默认，得 %d", got)
	}
	if got := sn.String(Aria2RPCURL, "fallback"); got != "http://127.0.0.1:6800/jsonrpc" {
		t.Errorf("应读到库里的字符串，得 %q", got)
	}
	if got := sn.String(Aria2DownloadDir, "fallback"); got != "fallback" {
		t.Errorf("字符串缺失应回默认，得 %q", got)
	}
}

// 写入是幂等的更新而不是重复插入（表有主键，但 ON CONFLICT 写错了会
// 在第二次保存时报 UNIQUE 约束 —— 用户看到的是"改第二次就报错"）。
func TestRepeatedApplyUpdatesInPlace(t *testing.T) {
	s := newStore(t, nil)
	for _, v := range []int{1, 2, 5} {
		put(t, s, map[string]any{string(MetricIntervalSec): v})
	}
	all, _ := s.All(context.Background())
	if got := find(t, all, MetricIntervalSec).Int(); got != 5 {
		t.Errorf("最终值应是 5，得 %d", got)
	}
}

// 空 PUT 只回全量，不动任何值。
func TestEmptyApplyIsNoOp(t *testing.T) {
	s := newStore(t, nil)
	put(t, s, map[string]any{string(StopGraceSec): 30})
	all, err := s.Apply(context.Background(), map[string]json.RawMessage{})
	if err != nil {
		t.Fatal(err)
	}
	if got := find(t, all, StopGraceSec).Int(); got != 30 {
		t.Errorf("空 PUT 不该改值，得 %d", got)
	}
}

// 采样间隔这项必须与 collector 接受的档位一致（1/2/5）。
// 这条测试本身不测 collector，它钉的是"注册表里的档位不要被人随手改"：
// 加一个 3 秒档需要同步改前端下拉，改错了就是界面与后端不一致。
func TestMetricIntervalMatchesDesignTiers(t *testing.T) {
	d, ok := lookup(MetricIntervalSec)
	if !ok {
		t.Fatal("采样间隔必须登记")
	}
	if len(d.Enum) != 3 || d.Enum[0] != 1 || d.Enum[1] != 2 || d.Enum[2] != 5 {
		t.Errorf("采样间隔档位应是 {1,2,5}，得 %v", d.Enum)
	}
}

// 每个数字项都必须有可用区间（Min<Max），否则这一项永远存不进去。
func TestEveryIntDefHasUsableRange(t *testing.T) {
	for _, d := range Defs() {
		switch d.Kind {
		case KindInt:
			if d.Min >= d.Max {
				t.Errorf("%s 区间无效: %d–%d", d.Key, d.Min, d.Max)
			}
			if d.Default != "" {
				n, err := strconv.Atoi(d.Default)
				if err != nil || n < d.Min || n > d.Max {
					t.Errorf("%s 默认值 %q 落在自己的区间之外", d.Key, d.Default)
				}
			}
		case KindEnum:
			if len(d.Enum) == 0 {
				t.Errorf("%s 是枚举但没有候选值", d.Key)
			}
			if d.Default != "" {
				n, _ := strconv.Atoi(d.Default)
				var hit bool
				for _, e := range d.Enum {
					if e == n {
						hit = true
					}
				}
				if !hit {
					t.Errorf("%s 默认值 %s 不在候选值里", d.Key, d.Default)
				}
			}
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 显式清空的可选字符串项 = 没填过（Set=false）。
// 若把"存过空串"算成 Set=true，输入框看着像保存了一个值而它其实是空的。
func TestExplicitlyClearedStringIsUnset(t *testing.T) {
	s := newStore(t, nil)
	put(t, s, map[string]any{string(Aria2DownloadDir): "/data/dl"})
	put(t, s, map[string]any{string(Aria2DownloadDir): ""})
	all, _ := s.All(context.Background())
	v := find(t, all, Aria2DownloadDir)
	if v.Set {
		t.Errorf("清空后不该算已设置: %+v", v)
	}
	if v.Value != "" {
		t.Errorf("值应是空串: %q", v.Value)
	}
}

// settings 表**同时**被 auth 用来存 password_hash（见 handlers_auth 的初始化
// 与 0001 迁移）。All() 只能返回注册表里登记的键。
//
// 如果实现"优化"成把整表都返回，GET /api/settings 就会把 bcrypt 摘要发给
// 浏览器 —— 摘要是密码的替身（拿到它就能离线爆破，也能直接拿去打某些
// 复用摘要做凭证的实现）。这条测试钉的是"白名单不是全表"这个结构决定，
// 而它在只剩一个 password_hash 行、看起来人畜无害的时候最容易被改写。
func TestNeverReturnsUnregisteredRows(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SqlDB().Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES('password_hash','$2a$12$fakefakefakefakefakefa',1)`); err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, nil, nil)
	all, err := s.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(registry) {
		t.Fatalf("返回了 %d 项，注册表只有 %d 项", len(all), len(registry))
	}
	for _, v := range all {
		if v.Key == "password_hash" {
			t.Fatal("password_hash 绝不能出现在设置项里")
		}
		if strings.Contains(v.Value, "$2a$") {
			t.Errorf("%s 的值里混进了密码摘要: %q", v.Key, v.Value)
		}
	}
}

// Kind 的 JSON 线格式必须是字符串。
//
// 这条测试存在的全部理由：Kind 曾经是 iota 枚举，而 encoding/json 对命名
// 整数类型编码出的是**数字**（KindEnum 编成 2，实测过），前端就只能硬编码
// 0/1/2 来选表单控件（数字框 / 下拉 / 文本框）；之后在中间插入一个新 Kind
// 会让前后端静默错位（旧数字指向新语义）。字符串常量错位不了，且 GET 响应
// 自带可读性。曾写过 Kind.String() 想绕开这点，但 json.Marshal 对命名整数
// 类型根本不调 Stringer —— 那是永不调用的死代码，已删。
//
// 直接 marshal 一个 Kind 而不是整个 Def：Def 里没有 json tag，字段名会变；
// 而它带着 Validate func（不可序列化），改天加个字段就会让这条测试因为
// 完全无关的原因变红。被测性质就一个：这个类型的线格式。
func TestKindSerializesAsString(t *testing.T) {
	for k, want := range map[Kind]string{
		KindInt:  `"int"`,
		KindStr:  `"string"`,
		KindEnum: `"enum"`,
	} {
		b, err := json.Marshal(k)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("Kind(%v) 应编码成 %s，得 %s（数字线格式会让前端在中间插项时静默错位）", k, want, b)
		}
	}
	// 注册表里每一项的 Kind 都必须是上面三种之一：出现空字符串或写错的
	// 字面量时，前端拿到一个认出的 kind 会渲染成什么完全看运气。
	valid := map[Kind]bool{KindInt: true, KindStr: true, KindEnum: true}
	for _, d := range Defs() {
		if !valid[d.Kind] {
			t.Errorf("%s 的 Kind=%q 不是已知类型", d.Key, d.Kind)
		}
	}
}
