/**
 * @Author: LiuShuXin
 * @Description: 日志组件配置与应用元数据校验
 * @File: options.go
 * Software: Goland
 * @Date: 2026/10/10 18:00
 */

package logging

import (
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

// Identity 定义应用名称和部署环境。
type Identity struct {
	Name        string `mapstructure:"name"`
	Environment string `mapstructure:"environment"`
}

// Options 定义日志输出和按自然日保留策略。
type Options struct {
	Level         string `mapstructure:"level"`
	Format        string `mapstructure:"format"`
	Color         string `mapstructure:"color"`
	Output        string `mapstructure:"output"`
	Directory     string `mapstructure:"directory"`
	TimeZone      string `mapstructure:"time_zone"`
	RetentionDays int    `mapstructure:"retention_days"`
}

// DefaultOptions 默认输出控制台，应用自行提供 Identity；文件输出需显式启用。
func DefaultOptions() Options {
	return Options{Level: "info", Format: "auto", Color: "auto", Output: "console",
		Directory: "logs", TimeZone: "UTC", RetentionDays: 7}
}

// Validate 校验应用名称和部署环境。
func (a Identity) Validate() error {
	var problems []error
	if strings.TrimSpace(a.Name) == "" {
		problems = append(problems, errors.New("app.name must not be empty"))
	}
	switch a.Environment {
	case "development", "test", "production":
	default:
		problems = append(problems, errors.New("app.environment must be development, test, or production"))
	}
	return errors.Join(problems...)
}

// Validate 校验日志输出、时区和保留天数。
func (l Options) Validate() error {
	var problems []error
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, errors.New("log.level must be debug, info, warn, or error"))
	}
	switch l.Format {
	case "auto", "console", "json":
	default:
		problems = append(problems, errors.New("log.format must be auto, console, or json"))
	}
	switch l.Output {
	case "console", "file", "both":
	default:
		problems = append(problems, errors.New("log.output must be console, file, or both"))
	}
	switch l.Color {
	case "auto", "always", "never":
	default:
		problems = append(problems, errors.New("log.color must be auto, always, or never"))
	}
	if strings.TrimSpace(l.Directory) == "" {
		problems = append(problems, errors.New("log.directory must not be empty"))
	}
	if l.TimeZone == "" || l.TimeZone == "Local" {
		problems = append(problems, errors.New("log.time_zone must be an explicit IANA time zone"))
	} else if _, err := time.LoadLocation(l.TimeZone); err != nil {
		problems = append(problems, fmt.Errorf("log.time_zone: %w", err))
	}
	if l.RetentionDays < 1 || l.RetentionDays > 36500 {
		problems = append(problems, errors.New("log.retention_days must be between 1 and 36500"))
	}
	return errors.Join(problems...)
}
