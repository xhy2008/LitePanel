package api

import (
	"net/http"
	"strings"

	"litepanel/internal/filemgr"
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
		// Accept-Ranges 要显式给：客户端是在**发 Range 之前**决定要不要
		// 续传的，等它发过来再告诉它就晚了（浏览器只会默默从头重下）。
		w.Header().Set("Accept-Ranges", "bytes")

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
