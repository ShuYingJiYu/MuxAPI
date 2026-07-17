// Package web 暴露编译进 Go 二进制的前端静态文件。
package web

import "embed"

// Dist 内嵌前端构建产物（需先 npm run build 生成 dist）。
//
//go:embed all:dist
var Dist embed.FS
