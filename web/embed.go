package web

import "embed"

// Dist 内嵌前端构建产物（需先 npm run build 生成 dist）。
//
//go:embed all:dist
var Dist embed.FS
