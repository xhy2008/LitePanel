package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"litepanel/internal/filemgr"
)

// 分块上传的三个端点（设计 8.2 + 17 节）。
//
// 参数为什么在头里：设计 17 节就是这么定的（X-Upload-Id / X-Chunk-Index），
// 而理由现在回头看很实在 —— 分块的 body 是文件的原始字节，没有任何空间
// 放元信息；用 multipart 包一层则要付一次解析开销与一次全量缓冲，而分块
// 大小是按"直接 stream 到磁盘"设计的。
//
// 因此多出 POST /api/fs/upload/begin：分块请求带不了"传到哪个目录、文件
// 叫什么、总共多大、同名怎么办"这四件必须在第一块之前定下来的事。设计
// 清单少写了一步，不是这里发明需求。

// 头名。集中成常量是为了避免三处字符串各写各的 —— 拼错的头不会报错，
// 只会静默走到"缺参数"分支。
const (
	uploadIDHeader    = "X-Upload-Id"
	chunkIndexHeader  = "X-Chunk-Index"
	maxBeginBodyBytes = 8 << 10 // begin 的请求体是几十字节的 JSON
)

// uploadBeginJSON 是 POST /api/fs/upload/begin 的请求体。
//
// 字段一律必填（除 conflict），校验交给 domain 层：这里再判一遍"size 不能
// 为负"只会让两处规则漂移。HTTP 层只负责它独有的判断 —— JSON 语法、
// 未知字段、以及 body 大小。
type uploadBeginJSON struct {
	ID       string `json:"id"`
	Dir      string `json:"dir"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Chunk    int64  `json:"chunk_size"`
	Conflict string `json:"conflict"`
}

func handleUploadBegin(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 限流不在这里：面板单用户，能过鉴权的就一个人。真正要防的是
		// "一个请求体写穿磁盘"，那是 chunk 端点的事（MaxChunkBytes）。
		r.Body = http.MaxBytesReader(w, r.Body, maxBeginBodyBytes)
		var in uploadBeginJSON
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的上传描述: "+err.Error())
			return
		}
		st, err := svc.BeginUpload(r.Context(), filemgr.UploadInit{
			ID: in.ID, Dir: in.Dir, Name: in.Name,
			Size: in.Size, ChunkSize: in.Chunk,
			Conflict: filemgr.Conflict(in.Conflict),
		})
		if err != nil {
			writeUploadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// handleUploadChunk 收一个分块。
//
// 注意它**不**是"上传"的唯一入口，也永远不该有人想把它当成唯一入口：
// 没有 begin 就没有会话，这里会老实地 404。
func handleUploadChunk(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 序号必须解析失败就报错，绝不默认 0。
		//
		// 默认 0 的写法看起来"宽容"，实际是把"前端漏发头"这个 bug 变成
		// 一个内容错位、却报告上传成功的文件 —— 用户拿到坏文件时不会
		// 怀疑到这里来。空串也走这条路（Atoi("") 报错），因为"没发头"
		// 与"发了个非数字"一样都没法猜出意图。
		raw := r.Header.Get(chunkIndexHeader)
		idx, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"缺少或非法的 "+chunkIndexHeader+" 头: "+raw)
			return
		}
		id := r.Header.Get(uploadIDHeader)
		if id == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "缺少 "+uploadIDHeader+" 头")
			return
		}
		st, err := svc.PutChunk(r.Context(), filemgr.UploadChunk{
			ID: id, Index: idx, Body: r.Body,
		})
		if err != nil {
			// 中途取消（客户端关页面、超时）不是服务端的错，也不值得
			// 前端弹红框：会话还在，下次续传。回 499 让日志能分清
			// "用户不传了"与"面板坏了"。
			writeUploadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func handleUploadStatus(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "uploadID")
		st, err := svc.UploadStatus(r.Context(), id)
		if err != nil {
			writeUploadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// handleUploadAbort 取消会话并丢掉已收的分块。
//
// 回 204 而不是 200 + 空 JSON：这里没有任何需要回给客户端的数据，
// 而"200 + {}"会让人以为有一个被清空了的资源仍然存在。
func handleUploadAbort(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := svc.AbortUpload(r.Context(), chi.URLParam(r, "uploadID")); err != nil {
			writeUploadError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeUploadError 把上传的领域错误翻成状态码。
//
// 与 writeFSError 分成两个函数而不是往里加 case：上传有自己的一批哨兵
// （ErrTooLarge / ErrChunk* / ErrChunkMismatch），混进文件系统那套会让
// 两边的注释互相解释，而它们的"该回什么"来自完全不同的理由。
//
// 顺序同 writeFSError：领域哨兵先判，底层 fs.ErrNotExist 兜在后面。
func writeUploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, filemgr.ErrTooLarge):
		// 413 + 出路。上限是产品决定（网页版是辅助功能，大文件走 SFTP），
		// 所以文案里必须把出路写出来：只说"太大"，用户只会反复点同一个
		// 按钮，因为他不知道多大算大、也不知道该怎么办。
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, filemgr.ErrChunkTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "chunk_too_large", err.Error())
	case errors.Is(err, filemgr.ErrChunkMismatch):
		// 409：同一块传进来不同内容。这是"客户端算错块"或"传输损坏"，
		// 静默采用后到的会产出一个内容鬼掉却报告成功的文件。
		writeError(w, http.StatusConflict, "chunk_mismatch", err.Error())
	case errors.Is(err, filemgr.ErrChunkIndex):
		writeError(w, http.StatusBadRequest, "chunk_index", err.Error())
	case errors.Is(err, filemgr.ErrChunkLength):
		writeError(w, http.StatusBadRequest, "chunk_length", err.Error())
	case errors.Is(err, filemgr.ErrBadPath):
		writeError(w, http.StatusBadRequest, "bad_path", err.Error())
	case errors.Is(err, filemgr.ErrExists):
		// begin 时目标已存在（选的是"询问"）。前端按这个 code 弹
		// "覆盖 / 改名 / 跳过"。
		writeError(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, filemgr.ErrUploadDone):
		// 同样是 409，但 code 必须不同：这里"换个名字"是完全错误的
		// 补救动作（文件已经传完了），要做的其实是删除。
		writeError(w, http.StatusConflict, "upload_done", err.Error())
	case errors.Is(err, filemgr.ErrNotDirectory):
		writeError(w, http.StatusBadRequest, "not_directory", err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "canceled", "客户端已断开")
	case errors.Is(err, fs.ErrPermission):
		writeError(w, http.StatusForbidden, "permission_denied", err.Error())
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "not_found", "没有这个上传会话（可能已过期或被取消）")
	default:
		writeErrorDetail(w, http.StatusInternalServerError, "fs_error",
			"上传失败", err.Error())
	}
}
