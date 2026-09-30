package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// 回收站的 HTTP 层（设计 702-705 行）+ 删除入口。
//
// 设计 17 节的端点表里**没有删除**：原本打算让删除走 M6-T4 的 jobs 异步
// 队列（复制/移动那种耗时操作才需要）。回收站改成按盘分置之后，删除是
// 一次同盘 rename(2)：瞬时、不占额外空间。把它塞进任务队列只会让用户
// 按下删除后先看到"任务已提交"再等一次轮询 —— 一个纯负的收益。所以
// 这里直接给 POST /api/fs/delete，jobs 里仍然保留复制/移动这类真需要
// 后台化的操作。
//
// 危险操作的二次确认一律服务端强制（设计 486 行；同 /term/sessions 的
// ?confirm=1）：只在前端弹确认框的话，一个脚本或一个被改过的前端就能
// 绕过，而"清空回收站"和"永久删除"都是不可逆的。

// handleFSDelete 处理 POST /api/fs/delete {paths:[...], permanent:bool}。
func handleFSDelete(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Paths     []string `json:"paths"`
			Permanent bool     `json:"permanent"`
		}
		if err := decodeStrict(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		n, err := svc.DeleteMany(r.Context(), in.Paths, in.Permanent)
		if err != nil {
			writeFSError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "permanent": in.Permanent})
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

// handleFSTrashPurge 处理 DELETE /api/fs/trash/{id}?confirm=1。
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
