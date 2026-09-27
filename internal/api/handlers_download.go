package api

import (
	"fmt"
	"net/http"
	"strings"

	"litepanel/internal/filemgr"
	"litepanel/internal/logx"
)

// 下载（设计 8.3）。
//
// 这里**故意不自己写 Range 逻辑**。理由不是省事：Range 的正确实现要处理
// 多区间、后缀区间、open-ended、越界、If-Range 与 ETag 的交互，自己写
// 一定漏，而每一种漏法的表现都是"下载下来的文件是坏的"且服务端零错误。
// http.ServeContent 这些都做完了，还顺带处理 HEAD 与 If-Range。
//
// 分工因此是：filemgr.Open 把"打开、判目录、拿头部三元组"做对，
// ServeContent 把已打开的 ReadSeeker 按 HTTP 语义流出去。

func handleFSDownload(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := queryPath(w, r)
		if !ok {
			return
		}
		f, err := svc.Open(r.Context(), p)
		if err != nil {
			writeFSError(w, err)
			return
		}
		defer f.File.Close()

		// 头必须在 ServeContent 之前设：它只在没写过头的情况下补默认值。
		w.Header().Set("Content-Disposition", contentDisposition(f.Name))
		w.Header().Set("Content-Type", downloadContentType(f.Name))
		// Accept-Ranges 不在这里设：serveContent 在它自己那行的前一行为
		// 所有 200/206 响应设好（GOROOT/src/net/http/fs.go 的
		// `w.Header().Set("Accept-Ranges", "bytes")`）。曾经在这里显式
		// 写过一次，理由写成"客户端发 Range 之前就得知道"—— 那个理由
		// 站不住：能发 Range 的响应正是已经带了这个头的响应。显式那行
		// 在 200 路径上是重复设同一个值，属于死代码，删。

		// 传 f.Name 而不是空串：ServeContent 会用这个名字猜 Content-Type，
		// 上面虽然已经设过，但让它保持一致也免于以后有人删掉上面那行时
		// 悄悄退化成 octet-stream。
		http.ServeContent(w, r, f.Name, f.ModTime, f.File)
	}
}

// downloadContentType 复用列表侧的扩展名表（见 filemgr.MimeOf），
// 再给文本类补上 charset。
//
// 补 charset 不是为了好看：手机浏览器偶尔会绕过 attachment 直接显示
// （某些 WebView 的"打开方式"、以及 Range+单字节的预览请求），没带
// charset 时按 RFC 2616 的默认值是 ISO-8859-1，中文日志文件会显示成
// 一串乱码 —— 而用户会以为是文件本身坏了。
//
// 只给 text/* 补：json/xml 的规范里 UTF-8 是默认编码，svg 是 XML，
// 给二进制类型加 charset 反而是错误的头。
func downloadContentType(name string) string {
	mt := filemgr.MimeOf(name, false)
	if strings.HasPrefix(mt, "text/") {
		return mt + "; charset=utf-8"
	}
	return mt
}

// contentDisposition 生成 attachment 头（RFC 6266 / 5987）。
//
// 两个名字都要有：
//
//   - filename*=UTF-8”<百分号编码>：非 ASCII 文件名的唯一正确写法。
//     HTTP 头按 Latin-1 解释，直接把 UTF-8 塞进 filename="报告.txt"，
//     Chrome 会按 Windows-1252 解码成"æ¥åå.txt"一类的乱码 —— 用户下载
//     50 个中文命名的备份，得到 50 个乱码文件，而他无从判断对应关系。
//   - filename="<纯 ASCII>"：老下载器与 curl --remote-header-name 只认
//     这一种，缺了它文件会被存成乱码或无扩展名（丢了扩展名等于丢了
//     "双击能不能打开"）。
//
// 非 ASCII 一律替换成 "_" 而不是音译：音译表要么内置（体积与维护成本），
// 要么依赖系统的 locale/ICU（目标机上有还是没有不确定）。ASCII 名的
// 用户看不出差别，非 ASCII 名的用户反正要靠 filename*= 拿到正确名字，
// 回退名只需要"能存下来、扩展名对"。
func contentDisposition(name string) string {
	// attr-char（RFC 5987）之外的字节全部百分号编码。表里剩下的这些
	// 字符在 URL 里是"永不需要转义"的集合，与 stdlib 的
	// shouldEscape 在 ext-value 语境下的判断一致。
	const attrChars = "!#$&+-.^_`|~"
	var ext strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte(attrChars, c) >= 0 {
			ext.WriteByte(c)
			continue
		}
		// 非 ASCII 字节（UTF-8 的多字节序列）在这里逐字节转义 —— 这正是
		// RFC 5987 要求的：编码对象是 UTF-8 字节串，不是码点。
		const hex = "0123456789ABCDEF"
		ext.WriteByte('%')
		ext.WriteByte(hex[c>>4])
		ext.WriteByte(hex[c&0xf])
	}

	var fb strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		// 引号、反斜杠、CR/LF 都会破坏头本身（响应头注入面），控制字符
		// 同理；非 ASCII 换成 _。
		if c > 0x20 && c < 0x7f && c != '"' && c != '\\' && c != ';' {
			fb.WriteByte(c)
			continue
		}
		// 路径分隔符理论上到不了这里（Name 是单个路径段），防御性保留：
		// 一旦哪天 Name 的来源变了，这里是最后一道不把 '/' 写进头的关。
		fb.WriteByte('_')
	}
	// 全非 ASCII 的名字会得到一长串 "_"，看着恶心但合法且能存下；
	// 空串则是非法头，所以绝不产生空（上面逐字节替换保证了这点）。

	return "attachment; filename=\"" + fb.String() + "\"; filename*=UTF-8''" + ext.String()
}

// 打包下载（设计 8.3：多选→ zip 流，不落临时文件）。
//
// ?path= 可以重复多次，前端把选中的行原样传上来即可 —— URL 里放列表下标
// 或 id 都需要前端与服务端同步"当前页"的状态，服务端无状态就没这个问题。

func handleFSZip(svc Files) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		paths := r.URL.Query()["path"]
		if len(paths) == 0 || (len(paths) == 1 && paths[0] == "") {
			writeError(w, http.StatusBadRequest, "bad_path", "没有选中任何文件")
			return
		}
		// 先逐个 Stat 再动笔。流式响应的头一旦写出就不能改，而"某个选中
		// 路径不存在"恰恰是最常见的一种失败（列表是几秒前加载的，
		// 期间别人删了它）。预检把这类可预见的失败挡在 200 之前，
		// 剩下的中途失败（EIO、读不了的子目录）才只能靠"坏 zip = 失败"
		// 兜底。多选常见几十项，Lstat 一次几百微秒，代价可以忽略。
		for _, p := range paths {
			if _, err := svc.Stat(r.Context(), p); err != nil {
				writeFSError(w, err)
				return
			}
		}

		// 下载名：多选时叫"多项 (N 项).zip"不如叫第一项 + "等 N 个"，
		// 用户是从某一行点的下载，那个名字是他唯一认得的锚点
		name := zipName(paths)
		w.Header().Set("Content-Disposition", contentDisposition(name))
		// application/zip 是登记过的媒体类型（RFC 9110 时代的
		// application/octet-stream 兜底会丢掉"这就是个压缩包"的信息，
		// 浏览器于是按扩展名猜，扩展名又被某些 WebView 忽略）
		w.Header().Set("Content-Type", "application/zip")

		// **不设 Content-Length**：流式打包的长度在结束前未知。手动设一个
		// 猜测值的话，Go 会按它截断或挂起，客户端得到一个坏 zip。
		// 分块传输是这里唯一诚实的选。
		if err := svc.Zip(r.Context(), paths, w); err != nil {
			// 头此时大概率已经发出去了（zip 的头几个字节就是响应体的开头），
			// **没有**"改成 500"这条路。唯一有用的动作是停止写流：
			// 客户端拿到的是缺中央目录的 zip，任何解压端都会判失败 ——
			// 失败但响亮，胜过悄悄少几个文件。
			logx.Info("zip 打包中断: %v", err)
			return
		}
	}
}

func zipName(paths []string) string {
	base := paths[0]
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base == "" {
		base = "下载"
	}
	if len(paths) == 1 {
		// 单文件打包也走这里：前端的多选框只勾中一个时发的就是 zip 请求，
		// 名字里丢掉扩展名会让它下完变成"无类型文件"
		if !strings.HasSuffix(strings.ToLower(base), ".zip") {
			base += ".zip"
		}
		return base
	}
	return base + fmt.Sprintf(" 等%d项.zip", len(paths))
}
