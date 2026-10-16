/**
 * @Author: LiuShuXin
 * @Description: 创建并发安全的 Zap 日志，统一格式、元信息及输出生命周期
 * @File: logging.go
 * Software: Goland
 * @Date: 2026/10/10 09:21
 */

// Package logging 创建由调用方管理的日志对象，不使用全局变量。
package logging

import (
	"io"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func newCore(levelName, format, environment string, output io.Writer, location *time.Location, colored bool) zapcore.Core {
	var level zapcore.Level
	_ = level.Set(levelName) // 配置已在创建日志对象前校验。
	if format == "auto" {
		format = "console"
		if environment == "production" {
			format = "json"
		}
	}

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "time"
	encoderConfig.MessageKey = "message"
	encoderConfig.EncodeTime = func(t time.Time, encoder zapcore.PrimitiveArrayEncoder) {
		encoder.AppendString(t.In(location).Format(time.RFC3339Nano))
	}
	var encoder zapcore.Encoder
	if format == "json" {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
		// 只改变开发控制台的级别编码，JSON 与生产环境不能被颜色开关绕过。
		if colored && environment == "development" {
			encoderConfig.EncodeLevel = coloredLevelEncoder
		}
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	// 输出加锁，保证派生日志对象共享普通 Writer 时也能安全并发写入。
	// 当前日志量无需采样，保留重复事件以便排查问题。
	return zapcore.NewCore(encoder, sink(output), level)
}

func coloredLevelEncoder(level zapcore.Level, encoder zapcore.PrimitiveArrayEncoder) {
	var color string
	switch level {
	case zapcore.DebugLevel:
		color = "\x1b[35m"
	case zapcore.InfoLevel:
		color = "\x1b[36m"
	case zapcore.WarnLevel:
		color = "\x1b[33m"
	case zapcore.ErrorLevel, zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel:
		color = "\x1b[31m"
	default:
		encoder.AppendString(level.CapitalString())
		return
	}
	// 级别文字后立即复位，避免正文、字段和堆栈继承颜色。
	encoder.AppendString(color + level.CapitalString() + "\x1b[0m")
}

func newLogger(app Identity, diagnostics io.Writer, cores ...zapcore.Core) *zap.Logger {
	return zap.New(zapcore.NewTee(cores...),
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
		zap.ErrorOutput(sink(diagnostics)),
		zap.Fields(zap.String("app", app.Name), zap.String("environment", app.Environment)),
	)
}

// Redacted 用占位符标记敏感字段，不接收原始值。
// 此方法不处理消息或错误中的敏感信息；禁止直接记录完整配置和请求体。
func Redacted(key string) zap.Field {
	return zap.String(key, "[REDACTED]")
}

func sink(writer io.Writer) zapcore.WriteSyncer {
	if file, ok := writer.(*os.File); ok && (file == os.Stdout || file == os.Stderr) {
		// 标准流直接写入，无本模块缓冲；终端和管道不一定支持 fsync，
		// 因此不执行文件同步，避免将不支持的操作误判为退出失败。
		writer = struct{ io.Writer }{writer}
	}
	// 自定义输出仍保留 Sync 能力，真实的刷新错误交给调用方处理。
	return zapcore.Lock(zapcore.AddSync(writer))
}
