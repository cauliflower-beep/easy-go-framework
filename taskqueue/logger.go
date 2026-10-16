/**
 * @Author: LiuShuXin
 * @Description: 隔离引擎任意格式化文本，保留组件结构化执行诊断
 * @File: logger.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import "go.uber.org/zap"

// 引擎日志参数可能包含任意异常文本、任务载荷及 Redis 脚本，不能直接拼接。
// 任务身份/失败分类由 executionError 记录，后端故障由 HealthCheckFunc 安全诊断。
type sdkLogger struct{ logger *zap.Logger }

func (l sdkLogger) Debug(...any) {}
func (l sdkLogger) Info(...any)  {}
func (l sdkLogger) Warn(...any)  { l.logger.Warn("taskqueue engine warning") }
func (l sdkLogger) Error(...any) { l.logger.Error("taskqueue engine error") }
func (l sdkLogger) Fatal(...any) { l.logger.Error("taskqueue engine fatal error") }
