package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"litepanel/internal/service"
)

// serviceView 是给前端的「定义 + 实时状态」合成视图。
//
// 字段一律 snake_case，与 Go tag 一一对应：前端不做驼峰转换，
// HTTP 与 WS 又共用同一套 key，改一处就要改两处必然漂移。
type serviceView struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Kind      service.Kind      `json:"kind"`
	Unit      string            `json:"unit"`
	StartCmd  string            `json:"start_cmd"`
	StopCmd   string            `json:"stop_cmd"`
	Cwd       string            `json:"cwd"`
	Autostart bool              `json:"autostart"`
	Sort      int               `json:"sort"`
	CreatedAt int64             `json:"created_at"`
	State     service.StateName `json:"state"`
	PID       int               `json:"pid"`
	StartedAt int64             `json:"started_at"`
	// 退出信息：exit_reason 为 null 表示从没退出过。
	// D21 只要三种呈现，这里就是全部依据：state / exit_reason / exit_code。
	ExitReason string `json:"exit_reason,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	ExitSignal *int   `json:"exit_signal,omitempty"`
	ExitAt     int64  `json:"exit_at,omitempty"`
	StoppedBy  string `json:"stopped_by,omitempty"`
}

func viewOf(svc service.Service, st service.State) serviceView {
	v := serviceView{
		ID: svc.ID, Name: svc.Name, Kind: svc.Kind, Unit: svc.Unit,
		StartCmd: svc.StartCmd, StopCmd: svc.StopCmd, Cwd: svc.Cwd,
		Autostart: svc.Autostart, Sort: svc.Sort, CreatedAt: svc.CreatedAt,
		State: st.State, PID: st.PID, StartedAt: st.StartedAt,
	}
	if v.State == "" {
		v.State = service.StateStopped
	}
	if st.Exit != nil {
		code, sig := st.Exit.Code, st.Exit.Signal
		v.ExitReason, v.StoppedBy = string(st.Exit.Reason), st.Exit.StoppedBy
		v.ExitCode, v.ExitSignal, v.ExitAt = &code, &sig, st.Exit.At
	}
	return v
}

// decodeServiceInput 读 body。PATCH 用它叠加到现有记录上，
// 所以只取真正出现在 JSON 里的字段。
type serviceInputJSON struct {
	Name      *string `json:"name"`
	Kind      *string `json:"kind"`
	Unit      *string `json:"unit"`
	StartCmd  *string `json:"start_cmd"`
	StopCmd   *string `json:"stop_cmd"`
	Cwd       *string `json:"cwd"`
	Autostart *bool   `json:"autostart"`
	Sort      *int    `json:"sort"`
}

func readServiceJSON(r *http.Request) (*serviceInputJSON, error) {
	defer r.Body.Close()
	var in serviceInputJSON
	// DisallowUnknownFields：前端打错字段名要立刻 400，
	// 静默忽略会让人以为"改了但没生效"。
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	return &in, nil
}

// pathID 解析 {id}。非数字与不存在都回 404：对前端而言
// "id 是垃圾"和"这条不存在"是同一件事，不必区分。
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "服务不存在")
		return 0, false
	}
	return id, true
}

func handleServicesList(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := service.List(deps.DB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "读取服务列表失败")
			return
		}
		out := make([]serviceView, 0, len(list)) // 绝不是 nil：前端直接 .map
		for _, svc := range list {
			st, err := service.GetState(deps.DB, svc.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "读取服务状态失败")
				return
			}
			out = append(out, viewOf(svc, st))
		}
		writeJSON(w, http.StatusOK, map[string]any{"services": out})
	}
}

func handleServicesCreate(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := readServiceJSON(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON："+err.Error())
			return
		}
		svc, err := service.Create(deps.DB, mergeInput(service.ServiceInput{}, raw))
		if err != nil {
			switch {
			case errors.Is(err, service.ErrDuplicateName):
				writeError(w, http.StatusConflict, "duplicate_name", "服务名已存在")
			default:
				writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			}
			return
		}
		st, _ := service.GetState(deps.DB, svc.ID)
		deps.serviceEvent(svc.ID, st.State)
		writeJSON(w, http.StatusOK, viewOf(svc, st))
	}
}

// mergeInput 把 PATCH 的部分字段叠到现有记录上。
// create=true 时以零值为底（缺省字段就是空），否则以库里已有的为底。
func mergeInput(base service.ServiceInput, raw *serviceInputJSON) service.ServiceInput {
	pick := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	pick(&base.Name, raw.Name)
	pick(&base.Kind, raw.Kind)
	pick(&base.Unit, raw.Unit)
	pick(&base.StartCmd, raw.StartCmd)
	pick(&base.StopCmd, raw.StopCmd)
	pick(&base.Cwd, raw.Cwd)
	if raw.Autostart != nil {
		base.Autostart = *raw.Autostart
	}
	if raw.Sort != nil {
		base.Sort = *raw.Sort
	}
	return base
}

func handleServicesUpdate(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		svc, err := service.Get(deps.DB, id)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		raw, err := readServiceJSON(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON："+err.Error())
			return
		}
		base := service.ServiceInput{
			Name: svc.Name, Kind: svc.Kind, Unit: svc.Unit, StartCmd: svc.StartCmd,
			StopCmd: svc.StopCmd, Cwd: svc.Cwd, Autostart: svc.Autostart, Sort: svc.Sort,
		}
		if err := service.Update(deps.DB, id, mergeInput(base, raw)); err != nil {
			switch {
			case errors.Is(err, service.ErrNotFound):
				writeError(w, http.StatusNotFound, "not_found", "服务不存在")
			case errors.Is(err, service.ErrDuplicateName):
				writeError(w, http.StatusConflict, "duplicate_name", "服务名已存在")
			default:
				writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			}
			return
		}
		out, _ := service.Get(deps.DB, id)
		st, _ := service.GetState(deps.DB, id)
		writeJSON(w, http.StatusOK, viewOf(out, st))
	}
}

// handleServicesDelete 运行中的服务先停再删。
// 否则进程变成面板里再也看不见的孤儿，是最难排查的一类泄漏。
func handleServicesDelete(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		svc, err := service.Get(deps.DB, id)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		if st, err := service.GetState(deps.DB, id); err == nil &&
			(st.State == service.StateRunning || st.State == service.StateStarting) {
			ctx, cancel := context.WithTimeout(r.Context(), service.DefaultGrace+8*time.Second)
			defer cancel()
			if _, err := deps.Services.Stop(ctx, svc, service.DefaultGrace); err != nil &&
				!errors.Is(err, service.ErrNotRunning) {
				writeError(w, http.StatusInternalServerError, "internal", "停止服务失败: "+err.Error())
				return
			}
		}
		if err := service.Delete(deps.DB, id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "删除失败")
			return
		}
		deps.serviceEvent(id, service.StateStopped)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleServiceToggle 一个按钮一个接口：跑着就停，停着就跑。
// 前端的磁贴本来就是单个开关，拆成两个 URL 只是把状态判断推给前端。
func handleServiceToggle(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		svc, err := service.Get(deps.DB, id)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		st, err := service.GetState(deps.DB, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "读取状态失败")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), service.DefaultGrace+10*time.Second)
		defer cancel()

		if st.State == service.StateRunning || st.State == service.StateStarting {
			out, err := deps.Services.Stop(ctx, svc, service.DefaultGrace)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "停止失败: "+err.Error())
				return
			}
			deps.serviceEvent(id, out.State)
			writeJSON(w, http.StatusOK, viewOf(svc, out))
			return
		}

		out, err := deps.Services.Start(svc)
		if err != nil {
			// 起不来是服务的错，不是请求格式错：409 比 500 更贴近事实。
			writeError(w, http.StatusConflict, "start_failed", "启动失败: "+err.Error())
			return
		}
		deps.serviceEvent(id, out.State)
		writeJSON(w, http.StatusOK, viewOf(svc, out))
	}
}

// logView 是内存环形缓冲的响应体（D19：日志不落盘，所以没有历史可查）。
type logView struct {
	Lines       []string `json:"lines"`
	CachedLines int      `json:"cached_lines"`
	BufferLimit int      `json:"buffer_limit"`
}

func handleServiceLog(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if _, err := service.Get(deps.DB, id); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		tail := service.DefaultLogLines
		if v := r.URL.Query().Get("tail"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeError(w, http.StatusBadRequest, "bad_request", "tail 必须是正整数")
				return
			}
			tail = n
		}
		buf := deps.Services.Log(id)
		lines := buf.Tail(tail)
		if lines == nil {
			lines = []string{}
		}
		writeJSON(w, http.StatusOK, logView{
			Lines: lines, CachedLines: buf.Len(), BufferLimit: buf.Limit(),
		})
	}
}

func handleServiceLogClear(deps AuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if _, err := service.Get(deps.DB, id); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		deps.Services.Log(id).Clear()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
