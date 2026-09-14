// Package webdist 把 Vite 构建产物 go:embed 进二进制。
package webdist

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist 返回以构建产物根目录为根的 fs.FS。
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
