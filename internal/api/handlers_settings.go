package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"litepanel/internal/settings"
)

// SettingsStore 是设置读写后端（生产由 *settings.Store 满足）。
//
// 收成接口是为了让 handler 能不带数据库就测完状态码映射与密钥打码 ——
// 而"密钥绝不出现在响应里"这一条恰恰是最不能只靠真库测的：真库测出来的
// 通过，可能只是因为测试数据恰好等于期望值。
type SettingsStore interface {
	All(ctx context.Context) ([]settings.Value, error)
	Apply(ctx context.Context, in map[string]json.RawMessage) ([]settings.Value, error)
}

// SettingsApply 在设置写库成功后被调用，把新值推给各个子系统热生效
// （装配层实现）。
type SettingsApply func(ctx context.Context) error

// settingItem 是 GET /api/settings 里的单项。
//
// **不能**直接序列化 settings.Value：它内嵌 Def，而 Def 带一个
// `Validate func(string) error` —— json.Marshal 遇到函数字段直接报错，
// 整个接口 500。即便哪天有人删了那个字段，把校验函数的有无当作 API 契约
// 的一部分也是错的。所以这里显式列一遍要暴露的字段。
type settingItem struct {
	Key   string `json:"key"`
	Group string `json:"group"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Unit  string `json:"unit,omitempty"`
	// Min/Max/Enum 是给前端渲染控件用的（数字框的范围、下拉的选项）。
	// 不发的话前端只能自己抄一份范围表，与注册表漂移之后会渲染出
	// 一个"允许填但后端拒收"的输入框。
	Min      int   `json:"min,omitempty"`
	Max      int   `json:"max,omitempty"`
	Enum     []int `json:"enum,omitempty"`
	Required bool  `json:"required,omitempty"`

	// Value 的类型随 Kind 变：数字项是 JSON number（前端表单要直接喂给
	// type=number 的输入框），字符串项是 string。
	//
	// 数字项**存坏了**（库里是非数字串）时这里会原样吐字符串并置 Invalid：
	// 夹到 0 的话用户在表单上看到的是一个从没人填过的 0，他一保存就把
	// 损坏固化成合法值。吐原值 + 标记，前端至少能提示"当前值不合法"。
	Value any  `json:"value"`
	Valid bool `json:"valid"`

	// Set 表示这项有非空值（可选字符串项清空与从未填过是同一件事）。
	Set bool `json:"set"`
	// Overridden 表示值来自库里（用户改过）而不是默认/config 兜底。
	// 前端用它标"已修改"，让用户看得出哪些项跟出厂值不一样。
	Overridden bool `json:"overridden"`

	// Secret 标记这是敏感项。它的 Value 恒为空串，真实值**绝不**出现在
	// 响应里（会进浏览器历史、Vue devtools、任何调试日志）。
	Secret bool `json:"secret,omitempty"`
	// HasValue 是密钥项唯一能暴露的事实：设过没有。前端据此显示
	// "已设置（留空则不修改）"，而不是把空输入框渲染成"没设过"。
	HasValue bool `json:"has_value,omitempty"`
	// RestartRequired：改完必须重启面板才生效，前端据此打标记。
	// 不标的话界面会显示“已保存”而值一个字节都没生效——那正是本项目对
	// 设置项定义的最坏失效模式。有了它，“这个改动要重启”至少是如实告知，
	// 而不是让用户自己踩。
	RestartRequired bool `json:"restart_required,omitempty"`
}

// settingGroup 是 GET 响应里的一个分组（前端据此渲染锚点导航）。
type settingGroup struct {
	Group string        `json:"group"`
	Items []settingItem `json:"items"`
}

// requiredKeys 是"值不能为空"的设置项。
//
// 为什么不在 settings 包里加一个 Required 字段然后照抄：必填与否目前就是
// "它的 Validate 会不会拒空串"，而 Go 没法从一个 func 反推这个性质；要么
// 在注册表里再存一个 bool（两处真相比一处更容易漂移），要么在这里列一份。
// 现在只有一个必填项（回收站目录名），列出来比给 Def 加字段更诚实，并由
// TestRequiredMatchesValidator 钉住"这里的集合 == 会拒空串的集合"。
func requiredKeys() map[settings.Key]bool {
	return map[settings.Key]bool{settings.TrashDirName: true}
}

// secretKeys 是 PUT 时要做"空串 = 不改"特殊处理的键集合。
func secretKeys() map[settings.Key]bool {
	m := map[settings.Key]bool{}
	for _, d := range settings.Defs() {
		if d.Secret {
			m[d.Key] = true
		}
	}
	return m
}

func toItem(v settings.Value) settingItem {
	it := settingItem{
		Key:             string(v.Key),
		Group:           v.Group,
		Kind:            string(v.Kind),
		Label:           v.Label,
		Unit:            v.Unit,
		Min:             v.Min,
		Max:             v.Max,
		Enum:            v.Enum,
		Set:             v.Set,
		Overridden:      v.Overridden,
		Secret:          v.Secret,
		RestartRequired: v.RestartRequired,
		Required:        requiredKeys()[v.Key],
		Valid:           true,
	}
	if v.Secret {
		// 只暴露"设过没有"，值本身一律不发。
		it.HasValue = v.Value != ""
		it.Set = it.HasValue
		it.Value = ""
		return it
	}
	switch v.Kind {
	case settings.KindInt, settings.KindEnum:
		n, err := strconv.Atoi(strings.TrimSpace(v.Value))
		if err != nil {
			// 坏值原样吐出去并标记不合法（理由见 Value 字段注释）。
			it.Value = v.Value
			it.Valid = false
			break
		}
		it.Value = n
	default:
		it.Value = v.Value
	}
	return it
}

// handleSettingsGet 返回按组分组的**全部**设置。
func handleSettingsGet(store SettingsStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vals, err := store.All(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "读取设置失败")
			return
		}
		// 按注册表顺序聚合成组（settings.Defs 已按 key 排过序，组的先后
		// 由首次出现决定）。前端不需要再排一次，也就没有"两边顺序不一致"
		// 这种只能靠肉眼发现的问题。
		var groups []settingGroup
		idx := map[string]int{}
		for _, v := range vals {
			it := toItem(v)
			i, ok := idx[it.Group]
			if !ok {
				idx[it.Group] = len(groups)
				groups = append(groups, settingGroup{Group: it.Group})
				i = len(groups) - 1
			}
			groups[i].Items = append(groups[i].Items, it)
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
	}
}

// handleSettingsPut 校验并写入一批设置，然后触发热生效。
//
// 请求体是扁平的 {key: value}，只带改过的那几项 —— 全量提交的表单一旦有
// 哪项没渲染出来就会把它的值悄悄清掉。
//
// 三件事按"必须先校验再动别的"的顺序排：
//  1. Apply 内部先全量校验再一次性写（有一项非法整批不写）；
//  2. 敏感项的空串按"不修改"处理（见下）；
//  3. 写成功后才调 Apply 回调推给子系统。
//
// 密钥项的**空串表示"不修改"**而不是"清空"：因为 GET 从不回显密钥，
// 表单里的密钥输入框在用户没动它时就是空的。如果照字面把空串写进去，
// 任何一次"只想改下载目录"的保存都会顺手把 aria2 的 RPC 密钥清掉 ——
// 用户看到的是"改了个目录，下载功能整个坏了"。真要清空密钥请改
// config.toml 后重启，这是有意留的摩擦。
func handleSettingsPut(store SettingsStore, apply SettingsApply) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体应是 {设置项: 值} 的 JSON 对象")
			return
		}
		secrets := secretKeys()
		for k, raw := range in {
			if !secrets[settings.Key(k)] {
				continue
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				// 密钥项只接受字符串。发别的一律拒，而不是猜个语义。
				writeError(w, http.StatusBadRequest, "bad_request", "敏感项必须是字符串")
				return
			}
			if s == "" {
				delete(in, k)
			}
		}
		vals, err := store.Apply(r.Context(), in)
		if err != nil {
			// 未知键与校验失败都是"用户/前端给的输入不对"，400 并带上
			// 原文：本项目禁止只回一句 failed（用户看不到哪一项错就没法改）。
			if errors.Is(err, settings.ErrUnknownKey) {
				writeError(w, http.StatusBadRequest, "unknown_key", err.Error())
			} else {
				writeError(w, http.StatusBadRequest, "invalid_setting", err.Error())
			}
			return
		}
		// 热生效失败**不**报 500：设置已经写进库了，报"失败"是撒谎，而
		// 用户会重复提交同一个已经生效的改动。回 200 并带上 applied:false
		// 与原因，界面可以显示"已保存，但部分项未能立即生效（可能需重启）"。
		applied := true
		var applyErr string
		if apply != nil {
			if err := apply(r.Context()); err != nil {
				applied = false
				applyErr = err.Error()
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"applied": applied,
			"reason":  applyErr,
			"groups":  groupItems(vals),
		})
	}
}

// groupItems 把一批 Value 聚成响应里的分组结构（与 GET 同一套规则，
// 保存后前端可以直接替换整棵树，不必自己 diff）。
func groupItems(vals []settings.Value) []settingGroup {
	var groups []settingGroup
	idx := map[string]int{}
	for _, v := range vals {
		it := toItem(v)
		i, ok := idx[it.Group]
		if !ok {
			idx[it.Group] = len(groups)
			groups = append(groups, settingGroup{Group: it.Group})
			i = len(groups) - 1
		}
		groups[i].Items = append(groups[i].Items, it)
	}
	return groups
}

// handleRevokeAllSessions 吊销全部登录会话（设置页"注销所有其他设备"）。
//
// 与改密码时的 RevokeAll 同一条路径。当前会话也一起吊销，所以响应里带
// reauth_required，前端据此跳登录页 —— 如果只吊销"别人"，用户其实无法
// 确认这件事真的发生了。
func handleRevokeAllSessions(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := deps.Sessions.RevokeAll(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "吊销会话失败")
			return
		}
		http.SetCookie(w, sessionCookie("", -1, deps.SecureCookie))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reauth_required": true})
	}
}
