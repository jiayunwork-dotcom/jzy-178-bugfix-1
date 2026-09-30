// Package apierr 定义带字段路径的输入校验错误。
//
// 所有对调用方输入做校验的包（lsq、profile、session 等）都返回 FieldError，
// 错误信息能直接指到出错的字段，HTTP 层据此返回 400。
package apierr

import "fmt"

// FieldError 指出某个字段（可为 JSON 路径，如 epochs[3].satellites[1].sigma）不合法。
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("字段 %s 非法: %s", e.Field, e.Reason)
}

// New 构造一个字段错误。
func New(field, reason string) *FieldError {
	return &FieldError{Field: field, Reason: reason}
}

// Sentinel 错误，用于需要按类型区分的非字段类问题（重复/倒退时间戳）。
type Sentinel string

func (e Sentinel) Error() string { return string(e) }

const (
	// ErrDuplicateEpoch 同一时间戳重复提交。
	ErrDuplicateEpoch Sentinel = "该历元时间戳已提交，状态不会重复推进"
	// ErrStaleEpoch 时间戳比会话最后一个历元更早。
	ErrStaleEpoch Sentinel = "历元时间戳倒退，已拒收"
	// ErrSessionNotFound 会话不存在。
	ErrSessionNotFound Sentinel = "会话不存在"
	// ErrProfileNotFound 运行档不存在。
	ErrProfileNotFound Sentinel = "运行档不存在"
)
