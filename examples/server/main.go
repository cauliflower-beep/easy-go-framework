/**
 * @Author: LiuShuXin
 * @Description: 演示默认配置、日志、HTTP、可选数据库及应用拥有的退出顺序
 * @File: main.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/config"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
	"github.com/cauliflower-beep/easy-go-framework/httpserver"
	"github.com/cauliflower-beep/easy-go-framework/logging"
	"github.com/cauliflower-beep/easy-go-framework/postgres"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type databaseSettings struct {
	Enabled    bool             `mapstructure:"enabled"`
	Connection postgres.Options `mapstructure:"connection"`
}

type settings struct {
	App      logging.Identity   `mapstructure:"app"`
	Log      logging.Options    `mapstructure:"log"`
	HTTP     httpserver.Options `mapstructure:"http"`
	Postgres databaseSettings   `mapstructure:"postgres"`
}

func defaultSettings() settings {
	return settings{
		App: logging.Identity{Name: "example-service", Environment: "development"},
		Log: logging.DefaultOptions(), HTTP: httpserver.DefaultOptions(),
		Postgres: databaseSettings{Connection: postgres.DefaultOptions()},
	}
}

func main() {
	path := flag.String("config", "", "optional YAML configuration path")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *path, os.Stdout, os.Stderr); err != nil {
		causes, truncated := logging.Diagnostics(err)
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"message": "example server failed", "causes": causes, "causes_truncated": truncated})
		os.Exit(1)
	}
}

func run(ctx context.Context, path string, output, diagnostics io.Writer) (err error) {
	var cfg settings
	if err = config.Load(path, &cfg, config.Options{EnvPrefix: "EXAMPLE", DefaultsFrom: defaultSettings()}); err != nil {
		return err
	}
	err = errors.Join(cfg.App.Validate(), cfg.Log.Validate(), cfg.HTTP.Validate())
	if cfg.Postgres.Enabled {
		err = errors.Join(err, cfg.Postgres.Connection.Validate())
	}
	if err != nil {
		return err
	}
	logs, err := logging.New(cfg.App, cfg.Log, output, diagnostics)
	if err != nil {
		return err
	}
	// 超时不证明后台函数退出。若入口没有排空，保留它仍可能使用的依赖，
	// 将错误交给进程入口决定退出，而不是继续销毁数据库和日志。
	logsMayClose, httpDrained := true, true
	defer func() {
		if logsMayClose {
			err = errors.Join(err, logs.Close())
		}
	}()
	logger, err := logs.Module("application")
	if err != nil {
		return err
	}
	var db *postgres.Database
	if cfg.Postgres.Enabled {
		dbLog, e := logs.Module("postgres")
		if e != nil {
			return e
		}
		db, err = postgres.Open(ctx, cfg.Postgres.Connection, dbLog)
		if err != nil {
			return err
		}
		defer func() {
			if !httpDrained {
				return
			}
			// 退出 Context 独立于已取消的信号 Context，等待上限由组件配置控制。
			closeErr := db.Shutdown(context.Background())
			if closeErr != nil {
				logsMayClose = false
			}
			err = errors.Join(err, closeErr)
		}()
	}
	httpLog, err := logs.Module("http")
	if err != nil {
		return err
	}
	var options []httpserver.Option
	if db != nil {
		options = append(options, httpserver.WithReadinessCheck(cfg.Postgres.Connection.HealthTimeout, db.Ping))
	}
	server, err := httpserver.New(cfg.HTTP, httpLog, options...)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := server.Shutdown(context.Background())
		if closeErr != nil {
			httpDrained, logsMayClose = false, false
		}
		err = errors.Join(err, closeErr)
	}()
	server.Router().GET("/example", httpapi.Handle(func(c *gin.Context) error {
		return httpapi.Success(c, gin.H{"message": "hello"})
	}))
	if db != nil {
		server.Router().GET("/database-time", httpapi.Handle(func(c *gin.Context) error {
			var now time.Time
			if err := db.WithSession(c.Request.Context(), func(session *gorm.DB) error {
				return session.Raw("SELECT CURRENT_TIMESTAMP").Scan(&now).Error
			}); err != nil {
				return err
			}
			return httpapi.Success(c, gin.H{"time": now.UTC()})
		}))
	}
	address, err := server.Start()
	if err != nil {
		return err
	}
	logger.Info("example server started", zap.String("address", address.String()), zap.Bool("postgres_enabled", db != nil))
	select {
	case <-ctx.Done():
		return nil
	case err = <-server.Errors():
		return err
	}
}
