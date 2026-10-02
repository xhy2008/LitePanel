package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"litepanel/internal/filemgr"
)

// 回收站的 HTTP 层（设计 702–705 行）+ 删除入口。
//
// 删除走的是队列，不是同步执行。这件事在两处设计文本里看起来矛盾，值得
// 写清楚：设计 705 行把 POST /api/fs/delete 单列成一个端点，而第 115 行
// 说"前端提交 copy/move/delete → 后端立即落库 job 并返回 job_id"，且没有
// 给小批量留同步分支。这里的取舍是：**路径保留**（M6-T5 的前端删除按钮
// 已经在调它，改路径只会白折腾一次前后端），**实现改成建任务**。于是
// /fs/delete 是 /fs/jobs 的一个特化：op 固定 delete、不带 dst。
//
// 为什么必须走队列而不是"删除很快，同步就行"：回收站按盘分置之后，单条
// 删除确实是一次同盘 rename(2)；但界面上一次框选可以是几万个路径（实测
// 约 220µs/条，5000 条约 1.1s，10 万条按目录树深度还要更久）。同步端点
// 里 r.Context() 会随关标签页、锁屏、代理超时取消，删除**就地停在一半**,
// 用户只看到一个网络错误，既不知道实际删了哪几个、也不知道剩下的仍在原地
// （实测中断时原地剩 2999/3000）。这恰恰是验收项"删除不因浏览器关闭
// 而中断"要防的事。
//
// 危险操作的二次确认一律服务端强制（设计 486 行；同 /term/sessions 的
// ?confirm=1）：只在前端弹确认框的话，一个脚本或一个被改过的前端就能
// 绕过，而"清空回收站"和"永久删除"都是不可逆的。

// handleFSDelete 处理 POST /api/fs/delete {paths:[...], permanent:bool}。
//
// 回 202 + job_id（与 /fs/jobs 同一形状），**不回**"已删除 N 条"。
//
// 受理阶段一个文件都不动，所以这里没有"存在性"可报：paths 里有一条不存在
// 时不再回 404，而是任务在执行时失败（整批不动，见 filemgr.deleteMany 的
// 先校验后执行）。这是同步改异步真实的、有意的体验变化 —— 报错从"立刻"
// 变成"任务列表里一条红色"。换来的那个保证更值钱：浏览器关掉，删除要么
// 整批做完要么整批不做。
func handleFSDelete(svc Jobs) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Paths     []string `json:"paths"`
			Permanent bool     `json:"permanent"`
		}
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		// 回收站建不起来的盘要在这里就说出口：前端靠这个哨兵码（422）
		// 把唯一可行的出路（永久删除，二次确认）摆给用户。
		if err := svc.PreflightTrash(r.Context(), in.Paths, in.Permanent); err != nil {
			writeJobError(w, err)
			return
		}
		j, err := svc.CreateJob(r.Context(), filemgr.JobInput{
			Op:        filemgr.OpDelete,
			Src:       in.Paths,
			Permanent: in.Permanent,
		})
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, jobResponse{Job: j, JobID: j.ID})
	}
}

// handleFSTrashList 处理 GET /api/fs/trash。
func handleFSTrashList(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := svc.ListTrash(r.Context())
		if err != nil {
			writeFSError(w, err)
			return
		}
		// 不在这里把 nil 兜成 []：领域层的 ListTrash 成功时恒返回非 nil
		// 切片（make + append），判空分支不可达。响应里 items 必须是 []
		// 而不是 null（前端 items.map 遇到 null 白屏）这条**契约**由
		// TestTrashListEmptyIsArray 在 JSON 层面钉住 —— 那是唯一正确的
		// 位置：将来领域层改成返回 nil，测试照样红，而这里多一层兜底
		// 反而会让红变成"实现改了而没人发现"。
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// handleFSTrashRestore 处理 POST /api/fs/trash/{id}/restore。
func handleFSTrashRestore(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		path, err := svc.RestoreTrash(r.Context(), id)
		if err != nil {
			writeFSError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": path})
	}
}

// handleFSTrashPurge 处理 DELETE /api/fs/trash/{id}?confirm=1（永久删一条）。
func handleFSTrashPurge(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireConfirm(w, r, "永久删除回收站条目") {
			return
		}
		if err := svc.PurgeTrash(r.Context(), chi.URLParam(r, "id")); err != nil {
			writeFSError(w, err)
			return
		}
		// 204：删除类操作没有内容可回，一个 body 只会让前端去猜 schema。
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleFSTrashEmpty 处理 POST /api/fs/trash/empty?confirm=1。
func handleFSTrashEmpty(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireConfirm(w, r, "清空回收站") {
			return
		}
		n, err := svc.EmptyTrash(r.Context())
		if err != nil {
			writeFSError(w, err)
			return
		}
		// 回条数：界面要显示"已清空 N 项"，而用户需要知道到底清了什么
		// 量级（清出 0 项本身就说明刚才看到的条目来自另一个没接上的盘）。
		writeJSON(w, http.StatusOK, map[string]any{"removed": n})
	}
}

// requireConfirm 实施服务端二次确认：危险操作必须带 ?confirm=1，否则
// 回 400 + confirm_required（同 /term/sessions 的删除，设计 486 行）。
//
// 只在前端弹确认框不够：任何漏改的调用、手写脚本或 curl 都会把不可逆
// 的操作直接执行掉，而没有任何一层会报错。已登录也不等于已确认 ——
// 会话在手机上可以挂七天，确认框的意义就是那一次主动点击。
//
// 为什么在 query 而不在 body：DELETE 带 body 在浏览器侧要多写一层
// fetch 处理，而 query 对 GET/DELETE/POST 一致，前端一个 helper 就够。
func requireConfirm(w http.ResponseWriter, r *http.Request, what string) bool {
	if r.URL.Query().Get("confirm") == "1" {
		return true
	}
	writeError(w, http.StatusBadRequest, "confirm_required", what+"是不可逆操作，需带 confirm=1")
	return false
}
