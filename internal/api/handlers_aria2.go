package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"litepanel/internal/download"
)

// aria2 下载模块的 HTTP 层（设计 751–760 行的端点表）。
//
// 文件名不用 handlers_download.go：那个名字已经被“文件下载”（ServeContent +
// zip）占了。两者是完全不同的东西 —— 一个是把服务器上的文件发给浏览器，
// 一个是面板指挥 aria2 从外网拉文件 —— 撞名会让人在改错的文件里找原因。
// 本文件的端点全部在 /api/dl/ 前缀下。
//
// 这一层最要紧的性质是**三类"没有数据"要给出三个不同的状态码**，因为用户的
// 下一步动作完全不同：
//
//	501            面板没接下载模块 → 面板自己的装配问题，用户做不了什么
//	503 + code     aria2 没装/没起  → 用户去装 aria2、或 systemctl start aria2
//	200 + 空列表    aria2 正常但没任务 → 用户该去点"新建下载"
//
// 把后两类混成"200 + 空列表"，前端就只能显示"还没有下载任务"，而真相是
// "aria2 根本没起来" —— 用户会反复点新建、每次失败、且没有任何地方告诉他为
// 什么。这与 Metrics/Files/Term 的 501 取舍是同一条规则的不同方向。
//
// /api/dl/health 是**唯一**一个不因 aria2 状态而失败的端点：它的职责就是报告
// aria2 状态，报告"没起来"是数据不是服务器错误。让它回 503 的话，前端通常把
// 它归进"请求失败"分支，于是安装引导永远出不来。

// Downloads 是下载端点需要的能力面（*download.Service 全量满足）。
type Downloads interface {
	Health(ctx context.Context) download.Health
	Summary(ctx context.Context) (download.Summary, error)
	Tasks(ctx context.Context) ([]download.TaskView, error)
	Add(ctx context.Context, in download.AddInput) (download.TaskView, error)
	Pause(ctx context.Context, gid string) error
	Resume(ctx context.Context, gid string) error
	Remove(ctx context.Context, gid string, force bool) error
	ClearHistory(ctx context.Context) (int, error)
}

var _ Downloads = (*download.Service)(nil)

// codeAria2Unavailable 是前端切到"安装引导"的判据。
//
// 必须是机器可判的 code 而不是中文文案：对文案做子串匹配在本项目里是禁止的
// （改一个标点就静默失效）。
const codeAria2Unavailable = "aria2_unavailable"

// writeDownloadError 把领域层错误映射成状态码。
//
// 分类顺序即优先级：
//  1. aria2 不可达 → 503 + aria2_unavailable。放最前：连接失败的错误文本里也
//     可能含别的关键词，先判哨最稳。
//  2. 面板不认识这个 gid → 404。前端的列表可能陈旧（另一个标签页清过历史），
//     404 才能触发它刷新列表而不是弹一句"操作失败"。
//  3. aria2 的业务拒绝 → 400 **并保留原文**。典型场景是对已完成的任务点
//     "继续"：吞掉原文只剩"操作失败"，用户无从判断是按钮点错了还是 aria2
//     坏了。
//  4. 其余 → 502：转发环节本身的故障（超时、连接被重置）。
func writeDownloadError(w http.ResponseWriter, err error) {
	switch {
	case download.IsUnavailable(err):
		// detail 带 err.Error()：引导文案要能直接显示，而里面含"哪个地址连不
		// 上"这种排障必需的信息，前端收起来放详情里正合适。
		writeErrorDetail(w, http.StatusServiceUnavailable, codeAria2Unavailable,
			"aria2 不可达：请确认已安装 aria2 并启动了 RPC 接口", err.Error())
	case errors.Is(err, download.ErrNoTask):
		writeError(w, http.StatusNotFound, "no_such_download", "任务不存在（可能已被清除，列表已过期）")
	default:
		var ae *download.Error
		if errors.As(err, &ae) {
			// aria2 的业务拒绝回 **400 而不是 502**。502 在 HTTP 语义上不算错
			// （确实是上游拒的），但它会被访问日志与监控统计成"面板故障"，而
			// 这一类几乎全是"对已完成的任务点了继续"这种用户层面的误操作。
			// 回 400 + aria2 原文，错误分类就只剩两种：面板的（校验/404）与
			// aria2 的（400 + 原文，原始 code 在 detail 里）。
			writeErrorDetail(w, http.StatusBadRequest, "aria2_rejected", ae.Message,
				fmt.Sprintf("aria2 code=%d method=%s", ae.Code, ae.Method))
			return
		}
		writeErrorDetail(w, http.StatusBadGateway, "download_failed", "操作下载失败", err.Error())
	}
}

// handleDLHealth 处理 GET /api/dl/health → 200（恒）。
func handleDLHealth(svc Downloads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// aria2 没起来不是错误，但"探不到"是（ctx 取消之类）—— 这里不细分：
		// HealthChecker 把 ctx 取消的原因也存进 Message，界面上至少看得到。
		writeJSON(w, http.StatusOK, svc.Health(r.Context()))
	}
}

// handleDLSummary 处理 GET /api/dl/summary。
func handleDLSummary(svc Downloads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := svc.Summary(r.Context())
		if err != nil {
			writeDownloadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	}
}

// handleDLTasks 处理 GET /api/dl/tasks → {tasks:[…]}.
//
// tasks 恒为数组：领域层已保证空时返回空切片而不是 nil，这里再兜一次是因为
// nil marshal 成 null 会让前端 data.tasks.length 抛 TypeError、整页白屏 ——
// 这个后果与"少一个字段"完全不成比例。
func handleDLTasks(svc Downloads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tasks, err := svc.Tasks(r.Context())
		if err != nil {
			writeDownloadError(w, err)
			return
		}
		if tasks == nil {
			tasks = []download.TaskView{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
	}
}

// 允许的 URL 协议。
//
// 是一个封闭集合而不是"含 :// 就放行"：不校验的话，用户把
// "www.example.com/a.zip"（漏了 https://）粘进来，aria2 会回一句
// "URI is not supported"，或者更糟 —— 把它当本地路径去读。面板自己校验能给出
// "缺少 http:// 或 https:// 前缀"这种可以直接照做的提示。
//
// 含 magnet/bt/ssh：aria2 认的这些都要放进来，否则校验会做成一个"比 aria2
// 更窄的白名单"，用户会以为自己填错了而实际是面板拦的。file:// 明确排除：那是
// 一个能读面板主机任意文件的路径，而本面板读文件有专门的、带鉴权与路径校验的
// 端点。
var allowedURLPrefixes = []string{"http://", "https://", "ftp://", "ftps://",
	"sftp://", "bt://", "magnet:"}

func allowedURI(u string) bool {
	low := strings.ToLower(strings.TrimSpace(u))
	for _, p := range allowedURLPrefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// handleDLAdd 处理 POST /api/dl/tasks → 201 {task}。
//
// 校验的分工：面板只校验**入参本身是否合法**（协议、目录存在性、split 范围），
// 资源可达性一律交给 aria2 —— 它才是执行方，而且"提交时能连上"到"真正开始
// 下载"之间那个窗口的判定本来就不可靠。
func handleDLAdd(svc Downloads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URIs  []string `json:"uris"`
			Dir   string   `json:"dir"`
			Out   string   `json:"out"`
			Split *int     `json:"split"`
		}
		// 畸形 JSON 一律 400 而不是 500：这是客户端的错，回 5xx 会让前端把它
		// 归进"服务器故障"、用户于是反复重试同一个坏请求。
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		if err := dec.Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 JSON 对象")
			return
		}
		uris, err := cleanURIs(in.URIs)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		dir, err := normalizeDownloadDir(in.Dir)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		split := 0
		if in.Split != nil {
			split = *in.Split
		}
		// 负数在面板侧归零：aria2 对负 split 的行为是静默按默认处理，用户会
		// 以为"多线程是假的"。上限夹在这里是为了让响应回显的值与真正提交的
		// 一致；领域层（download.Add）再夹一次，那道防线是给未来的调用方的。
		if split < 0 {
			split = 0
		}
		if split > download.MaxSplit {
			split = download.MaxSplit
		}

		task, err := svc.Add(r.Context(), download.AddInput{
			URIs: uris, Dir: dir, Out: strings.TrimSpace(in.Out), Split: split,
		})
		if err != nil {
			writeDownloadError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, task)
	}
}

// cleanURIs 去掉空串并校验协议。
//
// 空串要单独拦：aria2 收到 [""] 回的是 "URI is not provided."，一条既不知道
// 指的是第几项、又没法在界面上定位的错误；而前端"添加多个镜像地址"的表单很
// 容易留出一个空行。
func cleanURIs(raw []string) ([]string, error) {
	var out []string
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !allowedURI(u) {
			return nil, fmt.Errorf("地址 %s 协议不支持：需要 http://、https://、ftp:// 或 magnet:", truncForMsg(u))
		}
		// 结构完整性：scheme 之后什么都没有（"https://"）会被 aria2 收进队列
		// 然后立刻失败，错误还是一句英文原文。这里拦掉的代价是零。
		if strings.HasPrefix(strings.ToLower(u), "http") {
			if _, err := url.Parse(u); err != nil {
				return nil, fmt.Errorf("地址 %s 无法解析：%v", truncForMsg(u), err)
			}
			if rest := u[strings.Index(u, "://")+3:]; strings.TrimSpace(rest) == "" {
				return nil, fmt.Errorf("地址 %s 缺少主机名", truncForMsg(u))
			}
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, errors.New("至少需要一个下载地址")
	}
	return out, nil
}

// normalizeDownloadDir 校验保存目录。
//
// 空串原样返回（用 aria2 的 --dir 默认值）—— 这是"我不指定，你按配置来"，
// 拒掉它会让"新建下载"必须填一个路径，而大多数用户只有一个下载目录。
//
// 目录不存在时明确报错，而不是让 aria2 顺手 mkdir。aria2 **会**自动创建目标
// 目录，所以"不校验"也能跑通 —— 但打错一个字的结果是任务安安静静地往一个
// 新建出来的错误目录里下几十 GB，用户在预期位置找不到文件，还要面对一个凭空
// 多出来的目录。宁可拒绝并说明。
func normalizeDownloadDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if !strings.HasPrefix(dir, "/") {
		return "", errors.New("保存目录必须是绝对路径")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("保存目录不存在：%s", dir)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("保存目录不是一个目录：%s", dir)
	}
	return dir, nil
}

// truncForMsg 截断要回显给用户的输入。
//
// 回显是必要的（用户要知道自己哪一行填错了），但一条 200KB 的 magnet 链接
// 整条塞进错误响应既不实用也没意义。
func truncForMsg(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// handleDLControl 处理 POST /api/dl/tasks/{gid}/{pause|resume}。
func handleDLControl(svc Downloads, action func(context.Context, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gid := strings.TrimSpace(chi.URLParam(r, "gid"))
		if gid == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少任务 gid")
			return
		}
		if err := action(r.Context(), gid); err != nil {
			writeDownloadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "gid": gid})
	}
}

// handleDLRemove 处理 DELETE /api/dl/tasks/{gid}?force=1。
//
// force 是显式的：aria2 的强删会留下 .aria2 控制文件（下次同名下载会续传或
// 报错，取决于参数），这是需要用户明确选择的动作，不该默认发生。
func handleDLRemove(svc Downloads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gid := strings.TrimSpace(chi.URLParam(r, "gid"))
		if gid == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少任务 gid")
			return
		}
		force := isTrueFlag(r.URL.Query().Get("force"))
		if err := svc.Remove(r.Context(), gid, force); err != nil {
			writeDownloadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "gid": gid, "force": force})
	}
}

func isTrueFlag(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && b
}

// handleDLClearHistory 处理 DELETE /api/dl/history → {cleared:n}。
//
// 只删已终结的记录（领域层的 ClearHistory 保证）：清空历史把正在下的任务也
// 撤掉，是所有人都会踩一次的那种"我以为只是清列表"。
func handleDLClearHistory(svc Downloads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := svc.ClearHistory(r.Context())
		if err != nil {
			writeDownloadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": n})
	}
}
