/**
 * @Author: LiuShuXin
 * @Description: HTTP 路由装配、同步端口绑定、就绪状态及有界优雅停机
 * @File: server.go
 * Software: Goland
 * @Date: 2026/10/10 11:10
 */

// Package httpserver 负责 HTTP 基础设施，不依赖具体业务模块。
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cauliflower-beep/easy-go-framework/apperror"
	"github.com/cauliflower-beep/easy-go-framework/httpapi"
	"github.com/cauliflower-beep/easy-go-framework/logging"
)

// Option 在监听前装配运行期依赖，不进入配置加载器。
type Option func(*Server) error

// WithReadinessCheck 为依赖资源提供有期限的只读就绪检查。
// check 必须遵守 Context；所有依赖的具体实现由应用提供。
func WithReadinessCheck(timeout time.Duration, check func(context.Context) error) Option {
	return func(s *Server) error {
		if timeout <= 0 || check == nil {
			return errors.New("readiness timeout and check must be valid")
		}
		if s.readinessCheck != nil {
			return errors.New("readiness check already configured")
		}
		s.readinessTimeout = timeout
		s.readinessCheck = check
		return nil
	}
}

// Server 只允许启动一次；业务路由必须在 Start 前注册。
type Server struct {
	readinessCheck   func(context.Context) error
	readinessTimeout time.Duration
	probeFailed      atomic.Bool
	settings         Options
	logger           *zap.Logger
	router           *gin.Engine
	http             *http.Server
	cancel           context.CancelFunc
	ready            atomic.Bool
	mu               sync.Mutex
	started          bool
	stopping         bool
	listener         net.Listener
	serveErrors      chan error
	serveDone        chan struct{}
	shutdownOnce     sync.Once
	shutdownErr      error
	shutdownCtx      context.Context
	shutdownCancel   context.CancelFunc
	closed           chan struct{}
	forceOnce        sync.Once
	forceDone        chan struct{}
	forceErr         error
	connections      sync.WaitGroup
	requestMu        sync.Mutex
	active           int
	draining         bool
	drained          chan struct{}
}

// New 校验配置并装配中间件和基础路由，不占用网络端口。
func New(settings Options, logger *zap.Logger, options ...Option) (*Server, error) {
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("invalid HTTP configuration: %w", err)
	}
	if logger == nil {
		return nil, errors.New("HTTP logger must not be nil")
	}
	base, cancel := context.WithCancel(context.Background())
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	constructed := false
	defer func() {
		if !constructed {
			shutdownCancel()
		}
	}()
	s := &Server{
		settings: settings, logger: logger, cancel: cancel,
		serveErrors: make(chan error, 1), serveDone: make(chan struct{}), drained: make(chan struct{}),
		shutdownCtx: shutdownCtx, shutdownCancel: shutdownCancel, closed: make(chan struct{}), forceDone: make(chan struct{}),
	}
	for _, option := range options {
		if option == nil {
			cancel()
			return nil, errors.New("HTTP option must not be nil")
		}
		if err := option(s); err != nil {
			cancel()
			return nil, err
		}
	}
	close(s.drained)
	s.router = gin.New()
	s.router.HandleMethodNotAllowed = true
	// 交由 404 管道处理非规范路径，避免框架重定向绕过全局中间件。
	s.router.RedirectTrailingSlash = false
	s.router.RedirectFixedPath = false
	// 配置默认不信任代理头；只有明确指定的代理才能影响客户端 IP。
	if err := s.router.SetTrustedProxies(settings.TrustedProxies); err != nil {
		cancel()
		return nil, err
	}
	s.router.Use(s.trackRequests(), requestID(), accessLog(logger), httpapi.ErrorHandler(logger), recovery(logger), s.admission())
	if len(settings.CORS.AllowedOrigins) != 0 {
		c := cors.Config{
			AllowOrigins:     settings.CORS.AllowedOrigins,
			AllowMethods:     settings.CORS.AllowedMethods,
			AllowHeaders:     settings.CORS.AllowedHeaders,
			AllowCredentials: settings.CORS.AllowCredentials,
			ExposeHeaders:    []string{requestIDHeader},
			MaxAge:           settings.CORS.MaxAge,
		}
		if err := c.Validate(); err != nil {
			cancel()
			return nil, fmt.Errorf("invalid CORS configuration: %w", err)
		}
		s.router.Use(corsResponses(cors.New(c)))
	}
	s.router.Use(bodyLimit(settings.BodyLimit))
	s.router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "alive"}) })
	s.router.GET("/readyz", func(c *gin.Context) {
		if !s.ready.Load() {
			respondError(c, http.StatusServiceUnavailable, apperror.CodeNotReady, "service is not ready")
			return
		}
		if s.readinessCheck != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), s.readinessTimeout)
			err := s.readinessCheck(ctx)
			if err == nil {
				err = ctx.Err()
			}
			cancel()
			failed := err != nil
			previous := s.probeFailed.Swap(failed)
			// 仅记录依赖状态变化，避免探针故障时反复刷屏。
			if failed && !previous {
				s.logger.Warn("HTTP readiness dependency unavailable", logging.ErrorFields(err)...)
			}
			if !failed && previous {
				s.logger.Info("HTTP readiness dependency recovered")
			}
			if failed || !s.ready.Load() {
				respondError(c, http.StatusServiceUnavailable, apperror.CodeNotReady, "service is not ready")
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	s.router.NoRoute(func(c *gin.Context) { respondError(c, http.StatusNotFound, apperror.CodeNotFound, "route not found") })
	s.router.NoMethod(func(c *gin.Context) {
		respondError(c, http.StatusMethodNotAllowed, apperror.CodeMethodNotAllowed, "method not allowed")
	})
	s.http = &http.Server{
		Handler:           s.router,
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		ReadTimeout:       settings.ReadTimeout,
		WriteTimeout:      settings.WriteTimeout,
		IdleTimeout:       settings.IdleTimeout,
		MaxHeaderBytes:    settings.MaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return base },
		ErrorLog:          log.New(serverErrorWriter{logger}, "", 0),
		// Close 只关闭 socket，不等待连接服务协程。跟踪连接直到 net/http
		// 在 Handler/错误日志完成后报告 Closed，覆盖取消时尚未进入中间件的请求。
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				s.connections.Add(1)
			case http.StateClosed, http.StateHijacked:
				s.connections.Done()
			}
		},
	}
	constructed = true
	return s, nil
}

// Router 供组合根注册业务路由，运行后不得修改路由表。
func (s *Server) Router() *gin.Engine { return s.router }

// Start 先同步绑定端口，使启动错误直接返回，再异步执行 Serve。
func (s *Server) Start() (net.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopping {
		return nil, errors.New("HTTP server has already started or stopped")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(s.settings.Host, strconv.Itoa(s.settings.Port)))
	if err != nil {
		return nil, fmt.Errorf("listen HTTP: %w", err)
	}
	s.listener, s.started = listener, true
	s.ready.Store(true)
	go func() {
		err := s.http.Serve(listener)
		s.ready.Store(false)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.serveErrors <- err
		close(s.serveErrors)
		close(s.serveDone)
	}()
	return listener.Addr(), nil
}

// Errors 报告一次服务退出结果，nil 表示正常停机。
func (s *Server) Errors() <-chan error { return s.serveErrors }

func (s *Server) trackRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		s.requestMu.Lock()
		if s.active == 0 {
			s.drained = make(chan struct{})
		}
		s.active++
		s.requestMu.Unlock()
		defer func() {
			s.requestMu.Lock()
			s.active--
			if s.active == 0 {
				close(s.drained)
			}
			s.requestMu.Unlock()
		}()
		c.Next()
	}
}

func (s *Server) admission() gin.HandlerFunc {
	return func(c *gin.Context) {
		s.requestMu.Lock()
		draining := s.draining
		s.requestMu.Unlock()
		if draining {
			respondError(c, http.StatusServiceUnavailable, apperror.CodeShuttingDown, "service is shutting down")
			return
		}
		c.Next()
	}
}

func (s *Server) beginShutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		started := s.started
		s.ready.Store(false)
		s.mu.Unlock()
		s.requestMu.Lock()
		s.draining = true
		s.requestMu.Unlock()
		if !started {
			close(s.serveErrors)
			close(s.serveDone)
		}
		// 清理不绑定首位调用者的等待栈；重复调用可以继续等待真实收尾。
		go func() {
			var err error
			if started {
				err = s.http.Shutdown(s.shutdownCtx)
			}
			if s.shutdownCtx.Err() != nil {
				<-s.forceDone
				err = s.forceErr
			}
			<-s.serveDone
			// Serve 退出后不会再登记新连接；连接协程退出后也不会再产生新请求。
			s.connections.Wait()
			s.requestMu.Lock()
			drained := s.drained
			s.requestMu.Unlock()
			<-drained
			s.cancel()
			s.shutdownCancel()
			s.shutdownErr = err
			close(s.closed)
		}()
	})
}

func (s *Server) force() {
	s.forceOnce.Do(func() {
		s.cancel()
		s.shutdownCancel()
		go func() { s.forceErr = s.http.Close(); close(s.forceDone) }()
	})
}

func (s *Server) waitShutdown(ctx context.Context) error {
	select {
	case <-s.closed:
		return s.shutdownErr
	default:
	}
	select {
	case <-s.closed:
		return s.shutdownErr
	case <-ctx.Done():
		s.force()
		s.requestMu.Lock()
		active := s.active
		s.requestMu.Unlock()
		if active != 0 {
			return errors.Join(ctx.Err(), fmt.Errorf("%d HTTP handlers did not finish after cancellation", active))
		}
		return ctx.Err()
	}
}

// Shutdown 停止准入并等待请求及日志收尾，最长为 ctx 与 ShutdownTimeout
// 的较短者；超时取消 Context 并关闭连接，不额外延长等待。
// 超时不代表函数已退出，须保留依赖并可再次调用等待；只能在 Handler 外调用。
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("HTTP shutdown context must not be nil")
	}
	work, cancel := context.WithTimeout(ctx, s.settings.ShutdownTimeout)
	defer cancel()
	s.beginShutdown()
	return s.waitShutdown(work)
}

// Close 立即停止准入、取消请求并关闭连接，再按 ShutdownTimeout 有界等待。
// 正常退出使用 Shutdown；重复及并发调用可继续等待未退出的 Handler。
func (s *Server) Close() error {
	work, cancel := context.WithTimeout(context.Background(), s.settings.ShutdownTimeout)
	defer cancel()
	s.beginShutdown()
	s.force()
	if err := s.waitShutdown(work); err != nil {
		return err
	}
	select {
	case <-s.forceDone:
		return errors.Join(s.shutdownErr, s.forceErr)
	case <-work.Done():
		return work.Err()
	}
}

type serverErrorWriter struct{ logger *zap.Logger }

func (w serverErrorWriter) Write(data []byte) (int, error) {
	w.logger.Warn("HTTP server diagnostic", zap.String("detail", strings.TrimSpace(string(data))))
	return len(data), nil
}
