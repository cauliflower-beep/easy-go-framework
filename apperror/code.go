/**
 * @Author: LiuShuXin
 * @Description: 定义稳定数字错误码，区分框架保留范围与业务编码
 * @File: code.go
 * Software: Goland
 * @Date: 2026/10/13
 */
package apperror

// Code 使用 int32，JSON 输出为数字；0 成功，1..9999 为框架保留范围。
// 应用从 10000 起定义业务常量，已发布编码不重用；编码不等于 HTTP 状态码。
type Code int32

const (
	CodeOK                   Code = 0
	CodeInvalidArgument      Code = 1001
	CodeInvalidJSON          Code = 1002
	CodeInvalidQuery         Code = 1003
	CodeInvalidPath          Code = 1004
	CodeInvalidBody          Code = 1005
	CodeBodyTooLarge         Code = 1006
	CodeUnsupportedMediaType Code = 1007
	CodeMethodNotAllowed     Code = 1008
	CodeRequestTimeout       Code = 1009
	CodeUnauthenticated      Code = 1101
	CodeForbidden            Code = 1102
	CodeCORSForbidden        Code = 1103
	CodeNotFound             Code = 1201
	CodeConflict             Code = 1202
	CodeInternalError        Code = 1500
	CodeUnavailable          Code = 1501
	CodeNotReady             Code = 1502
	CodeShuttingDown         Code = 1503
)
