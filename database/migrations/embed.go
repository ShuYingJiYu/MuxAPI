// Package migrations 暴露按文件名排序执行的 PostgreSQL 迁移脚本。
package migrations

import "embed"

// Files contains the ordered PostgreSQL schema migrations.
//
//go:embed *.sql
var Files embed.FS
