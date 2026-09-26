package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"litepanel/internal/quickcmd"
)

// Commands 是 HTTP 层需要的全部快捷命令能力（M5-T9，设计 688-690 行）。
//
// 与 TermProber / TermSessions 不同，这里**没有**把"执行"拆成第二个接口：
// CRUD 与执行同属 quickcmd 一个模块、由同一处装配。拆成两个 Deps 字段就多
// 一个"接了一半"的失败模式 —— 列表能看、点就 501，而面板上一处报错都没有。
//
// 校验一律不在这里做：必填、空白、危险命令强制确认都属于 quickcmd 的
// normalized()（设计约束"校验只有一处主人"）。HTTP 层只做三件事：把 body
// 解成结构体（并拒绝未知字段）、把领域错误翻成状态码、把结果写成 JSON。
type Commands interface {
	List(ctx context.Context) ([]quickcmd.Command, error)
	Create(ctx context.Context, c quickcmd.Command) (quickcmd.Command, error)
	Update(ctx context.Context, id int64, in quickcmd.Command) error
	Delete(ctx context.Context, id int64) error
	Move(ctx context.Context, id int64, dir string) error
	Get(ctx context.Context, id int64) (quickcmd.Command, error)

	// Run 把命令投进一个终端会话（空闲的那个，或另开一个）。
	Run(ctx context.Context, c quickcmd.Command) (quickcmd.Result, error)
	// Busy 一次问完一批会话，供前端标"可投 / 忙"。返回的是**算好的结论**：
	// 判定窗口属于注入器（配置项），HTTP 层不参与，否则标签说的和注入时
	// 做的会相反。
	Busy(ctx context.Context, ids []int64) (map[int64]quickcmd.BusyInfo, error)
}

func handleCommandsList(s Commands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := s.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", "读取快捷命令失败")
			return
		}
		if items == nil {
			// 序列化成 []，不是 null：前端直接取 list.length，null 会抛
			// TypeError 让整个页面空白。
			items = []quickcmd.Command{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"commands": items})
	}
}

type commandJSON struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Cwd         string `json:"cwd"`
	NeedConfirm *bool  `json:"need_confirm"`
}

// toDomain 把 JSON 翻成领域结构体。
//
// need_confirm 用指针：POST 里"没写这个字段"和"写了 false"对**这一层**没有
// 区别（都交给领域层决定，危险命令会被强制 true），但指针能防止有人将来在
// 这里补一句 `if in.NeedConfirm == nil { ... }` 而把判定搬成第二处主人。
func (in commandJSON) toDomain() quickcmd.Command {
	c := quickcmd.Command{Name: in.Name, Command: in.Command, Cwd: in.Cwd}
	if in.NeedConfirm != nil {
		c.NeedConfirm = *in.NeedConfirm
	}
	return c
}

func handleCommandsCreate(s Commands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in commandJSON
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		cmd, err := s.Create(r.Context(), in.toDomain())
		if err != nil {
			cmdWriteErr(w, err, "创建失败")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"command": cmd})
	}
}

func handleCommandsUpdate(s Commands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "命令 id 不是数字")
			return
		}
		var in commandJSON
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if err := s.Update(r.Context(), id, in.toDomain()); err != nil {
			cmdWriteErr(w, err, "修改失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleCommandsDelete(s Commands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "命令 id 不是数字")
			return
		}
		if err := s.Delete(r.Context(), id); err != nil {
			cmdWriteErr(w, err, "删除失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

type commandMoveJSON struct {
	Dir string `json:"dir"`
}

func handleCommandsMove(s Commands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "命令 id 不是数字")
			return
		}
		var in commandMoveJSON
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		// 白名单挡在这里而不是存储里：存储收到非法方向只能猜（忽略还是
		// 报错），而 HTTP 层能直接给出 400 的原因。
		if in.Dir != "up" && in.Dir != "down" {
			writeError(w, http.StatusBadRequest, "bad_request", "dir 只能是 up 或 down")
			return
		}
		if err := s.Move(r.Context(), id, in.Dir); err != nil {
			cmdWriteErr(w, err, "排序失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleCommandsRun 点一下 → 投进终端会话。
//
// 为什么要服务端强制确认：确认框如果只在前端，任何漏改的调用、手写的脚本、
// curl 都会让 `rm -rf` 直接落到会话里，而没有任何一层会报错。**先查确认、
// 再执行**的顺序是这条守卫的全部内容 —— 执行完再回 400 等于没有守卫。
//
// 确认的判据是库里的 need_confirm，不是请求体：如果 POST body 能声明
// "这条不需要确认"，守卫就等于让调用方自己决定。所以这里必须先 Get 一次。
func handleCommandsRun(s Commands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "命令 id 不是数字")
			return
		}
		cmd, err := s.Get(r.Context(), id)
		if err != nil {
			cmdWriteErr(w, err, "读取命令失败")
			return
		}
		if cmd.NeedConfirm && r.URL.Query().Get("confirm") != "1" {
			writeError(w, http.StatusBadRequest, "confirm_required",
				"该命令标记为需确认，请带 confirm=1 重试")
			return
		}
		res, err := s.Run(r.Context(), cmd)
		if err != nil {
			cmdWriteErr(w, err, "执行失败")
			return
		}
		// session_id / is_new_session 是前端跳终端页 + 选标签 + 决定 toast
		// 文案的全部依据。
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id":     res.SessionID,
			"is_new_session": res.IsNewSession,
			"title":          res.Title,
		})
	}
}

// handleCommandsBusy 一次返回一批会话的忙闲。
//
// 参数是会话 id 而不是命令 id：忙的是终端会话，与命令无关。前端把当前所有
// 标签的 id 传一次即可，逐个请求就是 N 倍往返，而每次往返在 tmux 上都是
// 一次 fork（四个标签四倍开销）。
//
// 没有 ids 时回空列表而不是报错：页面刚打开、一个会话都没有是正常状态。
func handleCommandsBusy(s Commands) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var ids []int64
		for _, raw := range q["session"] {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "session 参数不是数字")
				return
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"sessions": []any{}})
			return
		}
		states, err := s.Busy(r.Context(), dedupe(ids))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "tmux_error", "查询会话状态失败")
			return
		}
		out := make([]map[string]any, 0, len(states))
		for id, st := range states {
			out = append(out, map[string]any{
				"session_id": id,
				"busy":       st.Busy,
				"foreground": st.Foreground,
				"shell_name": st.ShellName,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	}
}

// cmdWriteErr 把领域错误翻成状态码。未知错误一律 500，不猜。
func cmdWriteErr(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, quickcmd.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "快捷命令不存在")
	case errors.Is(err, quickcmd.ErrNameRequired), errors.Is(err, quickcmd.ErrCommandRequired):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "cmd_error", fallback)
	}
}

// dedupe 去掉重复 id，保持首次出现的顺序。
//
// 保持顺序是因为响应要画进标签栏，顺序乱了前端还得再排一次。
func dedupe(in []int64) []int64 {
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
