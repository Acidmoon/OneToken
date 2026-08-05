// Package store 是 SQLite 数据层（设计文档 §4）。
//
// 连接约定（M1.1 实现）：PRAGMA foreign_keys=ON + journal_mode=WAL
// + busy_timeout=5000；时间戳统一 UTC Z 后缀；raw 行只 INSERT 不 UPDATE。
//
// 迁移约定：本文件为迁移骨架（P0.2），M1.1 填充实际 schema。
// 迁移必须幂等（可重复执行不报错）；schema 变更记录 schema_version。
package store

// Migration 描述一次幂等 schema 迁移。
type Migration struct {
	Version int    // 单调递增
	Name    string // 迁移名
	SQL     string // 幂等 SQL（IF NOT EXISTS 等）
}

// migrations 是迁移序列（P0.2 骨架：空表，M1.1 追加）。
var migrations = []Migration{}
