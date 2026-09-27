package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"litepanel/internal/terminal"
)

// TermProber 是 HTTP 层需要的全部终端健康能力。
//
// 只有 Health() 一个方法，和 MetricsSource 只暴露 OnDemand() 是同一个理由：
// "tmux 到底能不能用"这件事的判定（版本下限、输出格式、保守方向）属于
// internal/terminal，HTTP 层不参与判断，只负责把它翻译成 JSON。
//
// 实现者应当**缓存**探测结果：`tmux -V` 虽然瞬时，但每次请求 fork 一个子进程
// 仍然没必要（tmux 不会在面板运行期间自己装上或升级）。有测试钉住"一次请求
// 只读一次"。
type TermProber interface {
	Health() terminal.Health
}

// handleTermHealth 返回 tmux 可用性（设计 7.3）。
//
// 注意状态码的取舍：tmux **不可用也回 200**。"没装 tmux"是面板运行环境的
// 一种正常状态，不是本次请求的失败；前端要据 available=false 渲染安装引导，
// 而 4xx/5xx 会走统一错误体那条路径，引导框就永远出不来。
// 对照 handleMetricsSnapshot 回 500 的理由：采集器报错是**意外**故障，
// 用 200 空壳会渲染成"各项 0%"的假数据；而 tmux 缺失有专门的 UI 承接。
func handleTermHealth(p TermProber) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, p.Health())
	}
}

// ---- 终端会话 CRUD（M5-T6，设计 682-686 行）----

// TermSessions 是 HTTP 层需要的全部终端会话能力。
//
// 与 TermProber 分开是刻意的：健康探测只读、无副作用、可缓存；会话 CRUD
// 会创建/杀掉 tmux 会话。合成一个接口会让"只想报个版本号"的测试桩被迫
// 实现五个杀进程的方法，也就没人愿意给真实现写装配测试了。
type TermSessions interface {
	List(ctx context.Context) ([]terminal.SessionMeta, error)
	Create(ctx context.Context, in terminal.SessionInput) (terminal.SessionMeta, error)
	Rename(ctx context.Context, id int64, title string) error
	Delete(ctx context.Context, id int64) error
	CorpseOutput(ctx context.Context, id int64) (string, error)
}

// handleTermSessionsList 返回会话列表（含 tmux 里已消失的，alive=false）。
//
// 为什么不隐藏已死的会话：用户在 tmux 里 exit 之后，面板里那行标签还在
// 才是符合直觉的 —— 消失会让他以为面板吞了他的会话，而"已退出"是一个
// 可以解释、可以顺手删掉的状态。
func handleTermSessionsList(s TermSessions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := s.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", "读取会话列表失败")
			return
		}
		if items == nil {
			items = []terminal.SessionMeta{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
	}
}

type termCreateJSON struct {
	Title        string `json:"title"`
	Cwd          string `json:"cwd"`
	Shell        string `json:"shell"`
	HistoryLimit int    `json:"history_limit"`
}

// handleTermSessionsCreate 新建会话。
//
// 字段校验全部交给存储层的 normalized()：白名单、必填只有一处主人。
// 这里只负责把它翻成 400，以及把"未知字段"挡在门外（前端打错字段名要
// 立刻失败，静默忽略会让人以为"设置了但没生效"）。
func handleTermSessionsCreate(s TermSessions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in termCreateJSON
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		sess, err := s.Create(r.Context(), terminal.SessionInput{
			Title: in.Title, Cwd: in.Cwd, Shell: in.Shell, HistoryLimit: in.HistoryLimit,
		})
		if err != nil {
			termWriteErr(w, err, "创建会话失败")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"session": sess})
	}
}

type termRenameJSON struct {
	Title *string `json:"title"`
}

// handleTermSessionsRename 只接受 title。
//
// cwd / shell / history_limit 在建会话那一刻就固化进 tmux 了，PATCH 它们
// 要么得重建会话、要么得改 tmux 全局配置，都不是 PATCH 该悄悄做的事 ——
// 加上 DisallowUnknownFields，传了就 400。
func handleTermSessionsRename(s TermSessions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "会话 id 不是数字")
			return
		}
		var in termRenameJSON
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if in.Title == nil {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少 title")
			return
		}
		if err := s.Rename(r.Context(), id, *in.Title); err != nil {
			termWriteErr(w, err, "重命名失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleTermSessionsDelete 删会话（连 tmux 会话一起杀）。
//
// 必须带 ?confirm=1。这是**服务端**强制的二次确认：只在前端弹确认框的话，
// 任何漏改的调用、手写的脚本、curl 都会把一个正在跑任务的会话连 shell
// 一起杀掉，而且没有任何一层会报错。
func handleTermSessionsDelete(s TermSessions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := chiID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "会话 id 不是数字")
			return
		}
		if r.URL.Query().Get("confirm") != "1" {
			writeError(w, http.StatusBadRequest, "confirm_required",
				"删除会话会终止其中正在运行的程序，需带 confirm=1")
			return
		}
		if err := s.Delete(r.Context(), id); err != nil {
			termWriteErr(w, err, "删除会话失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// termWriteErr 把领域错误翻成状态码。未知错误一律 500，不猜。
func termWriteErr(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, terminal.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "not_found", "会话不存在")
	case errors.Is(err, terminal.ErrBadHistoryLimit):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, terminal.ErrTitleRequired):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "term_error", fallback)
	}
}

// decodeStrict 解 body 并拒绝未知字段。
func decodeStrict(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func chiID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// handleTermSessionOutput 返回会话的最后输出（"遗言"）。
//
// 空 output + 200 是有效语义："这个会话没有可读的历史"（消失的尸体
// 没有 grid）。前端据此显示"无输出记录"，而不是弹一个红色的加载失败。
func handleTermSessionOutput(svc TermSessions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		txt, err := svc.CorpseOutput(r.Context(), id)
		if err != nil {
			termWriteErr(w, err, "读取会话输出失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"output": txt})
	}
}
