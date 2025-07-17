// Package core contains the main struct of the software.
// 核心包，包含MediaMTX软件的主要结构体
package core

import (
	"context"
	_ "embed" // 用于嵌入编译时文件
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/alecthomas/kong"         // 命令行参数解析库
	"github.com/bluenviron/gortsplib/v4" // RTSP协议库
	"github.com/gin-gonic/gin"           // Web框架

	// 内部包导入
	"github.com/bluenviron/mediamtx/internal/api"
	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/confwatcher"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/metrics"
	"github.com/bluenviron/mediamtx/internal/playback"
	"github.com/bluenviron/mediamtx/internal/pprof"
	"github.com/bluenviron/mediamtx/internal/recordcleaner"
	"github.com/bluenviron/mediamtx/internal/rlimit"
	"github.com/bluenviron/mediamtx/internal/servers/hls"
	"github.com/bluenviron/mediamtx/internal/servers/rtmp"
	"github.com/bluenviron/mediamtx/internal/servers/rtsp"
	"github.com/bluenviron/mediamtx/internal/servers/srt"
	"github.com/bluenviron/mediamtx/internal/servers/webrtc"
)

//go:generate go run ./versiongetter
// 生成版本信息的指令

//go:embed VERSION
var version []byte // 嵌入编译时生成的版本号文件

// 默认配置文件查找路径，按优先级排序
// 支持多种部署场景（开发环境、系统安装等）
var defaultConfPaths = []string{
	"rtsp-simple-server.yml",      // 兼容旧版本配置文件名
	"mediamtx.yml",                // 当前版本配置文件名
	"/usr/local/etc/mediamtx.yml", // 本地安装配置
	"/usr/etc/mediamtx.yml",       // 系统级配置
	"/etc/mediamtx/mediamtx.yml",  // 标准系统配置
}

// 命令行参数结构体，定义程序启动时的参数
var cli struct {
	Version  bool   `help:"print version"` // --version 显示版本信息
	Confpath string `arg:"" default:""`    // 配置文件路径（位置参数）
}

// 检查是否有路径配置了录制后自动删除功能
// 用于决定是否需要启动录制清理器
func atLeastOneRecordDeleteAfter(pathConfs map[string]*conf.Path) bool {
	for _, e := range pathConfs {
		if e.RecordDeleteAfter != 0 {
			return true
		}
	}
	return false
}

// 计算RTP最大负载大小，考虑加密和MTU限制
// 用于优化网络传输性能
func getRTPMaxPayloadSize(udpMaxPayloadSize int, rtspEncryption conf.Encryption) int {
	// UDP最大负载大小 - 12字节（RTP头部）
	v := udpMaxPayloadSize - 12

	// 如果启用SRTP加密，需要减去10字节（SRTP HMAC SHA1认证标签）
	if rtspEncryption == conf.EncryptionOptional || rtspEncryption == conf.EncryptionStrict {
		v -= 10
	}

	return v
}

// Core结构体是MediaMTX的全局调度器，负责：
// - 生命周期管理（启动、运行、关闭）
// - 配置管理（加载、热重载）
// - 子系统协调（各协议服务器、API、监控等）
// - 资源管理（内存、文件描述符等）
type Core struct {
	ctx             context.Context          // 全局上下文，控制所有协程的生命周期
	ctxCancel       func()                   // 取消函数，用于优雅关闭
	confPath        string                   // 配置文件路径
	conf            *conf.Conf               // 当前配置对象
	logger          *logger.Logger           // 日志系统
	externalCmdPool *externalcmd.Pool        // 外部命令池（用于执行钩子命令）
	authManager     *auth.Manager            // 认证与权限管理
	metrics         *metrics.Metrics         // Prometheus监控指标
	pprof           *pprof.PPROF             // 性能分析
	recordCleaner   *recordcleaner.Cleaner   // 录制文件清理器
	playbackServer  *playback.Server         // 回放HTTP服务器
	pathManager     *pathManager             // 路径管理器（流的核心调度）
	rtspServer      *rtsp.Server             // RTSP服务器（明文）
	rtspsServer     *rtsp.Server             // RTSPS服务器（加密）
	rtmpServer      *rtmp.Server             // RTMP服务器（明文）
	rtmpsServer     *rtmp.Server             // RTMPS服务器（加密）
	hlsServer       *hls.Server              // HLS服务器
	webRTCServer    *webrtc.Server           // WebRTC服务器
	srtServer       *srt.Server              // SRT服务器
	api             *api.API                 // 控制API
	confWatcher     *confwatcher.ConfWatcher // 配置文件热重载监听器

	// 输入通道
	chAPIConfigSet chan *conf.Conf // API配置变更通道

	// 输出通道
	done chan struct{} // 关闭信号通道
}

// New allocates a Core.
// New函数是MediaMTX的全局构造函数，负责：
// 1. 解析命令行参数
// 2. 加载配置文件
// 3. 初始化所有子系统（日志、认证、API、各协议服务器等）
// 4. 启动主事件循环
// 返回值：(*Core, bool) - Core实例和是否成功
func New(args []string) (*Core, bool) {
	// 创建命令行参数解析器
	parser, err := kong.New(&cli,
		kong.Description("MediaMTX "+string(version)), // 设置程序描述
		kong.UsageOnError(),                           // 解析错误时显示使用说明
		kong.ValueFormatter(func(value *kong.Value) string {
			switch value.Name {
			case "confpath":
				return "path to a config file. The default is mediamtx.yml."
			default:
				return kong.DefaultHelpValueFormatter(value)
			}
		}))
	if err != nil {
		panic(err)
	}

	// 解析命令行参数
	_, err = parser.Parse(args)
	parser.FatalIfErrorf(err)

	// 如果指定了--version参数，显示版本信息并退出
	if cli.Version {
		fmt.Println(string(version))
		os.Exit(0)
	}

	// 创建全局上下文，用于控制所有协程的生命周期
	ctx, ctxCancel := context.WithCancel(context.Background())

	// 创建Core实例，初始化基础字段
	p := &Core{
		ctx:            ctx,                   // 全局上下文
		ctxCancel:      ctxCancel,             // 取消函数
		chAPIConfigSet: make(chan *conf.Conf), // API配置变更通道
		done:           make(chan struct{}),   // 关闭信号通道
	}

	// 创建临时日志器，用于配置文件加载过程中的错误输出
	tempLogger, _ := logger.New(logger.Warn, []logger.Destination{logger.DestinationStdout}, "", "")

	// 加载配置文件，优先使用命令行参数指定的路径，否则使用默认路径列表
	p.conf, p.confPath, err = conf.Load(cli.Confpath, defaultConfPaths, tempLogger)
	if err != nil {
		fmt.Printf("ERR: %s\n", err)
		return nil, false
	}

	// 初始化所有子系统（日志、认证、API、各协议服务器等）
	err = p.createResources(true)
	if err != nil {
		// 如果初始化失败，记录错误并清理资源
		if p.logger != nil {
			p.Log(logger.Error, "%s", err)
		} else {
			fmt.Printf("ERR: %s\n", err)
		}
		p.closeResources(nil, false)
		return nil, false
	}

	// 启动主事件循环（监听配置变更、信号等）
	go p.run()

	return p, true
}

// Close closes Core and waits for all goroutines to return.
// 主动关闭Core实例，取消上下文并等待所有协程退出
// 这是优雅关闭的关键函数，确保资源正确释放
func (p *Core) Close() {
	p.ctxCancel() // 取消全局上下文，通知所有协程开始关闭
	<-p.done      // 等待主事件循环退出
}

// Wait waits for the Core to exit.
// 阻塞等待Core实例退出
// 通常在主程序中调用，等待程序自然结束
func (p *Core) Wait() {
	<-p.done // 等待主事件循环退出信号
}

// Log implements logger.Writer.
// 实现logger.Writer接口，提供统一的日志输出
// 所有子系统都可以通过这个接口记录日志
func (p *Core) Log(level logger.Level, format string, args ...interface{}) {
	p.logger.Log(level, format, args...)
}

// 主事件循环，负责监听和处理各种事件
// 这是Core的核心调度器，管理整个程序的生命周期
func (p *Core) run() {
	defer close(p.done) // 确保在函数退出时关闭done通道

	// 获取配置文件变更监听通道
	// 如果配置监听器存在，返回其监听通道；否则返回空通道
	confChanged := func() chan struct{} {
		if p.confWatcher != nil {
			return p.confWatcher.Watch()
		}
		return make(chan struct{})
	}()

	// 创建系统中断信号监听器
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt) // 监听Ctrl+C信号

outer:
	for {
		select {
		case <-confChanged:
			// 配置文件发生变更（文件被修改）
			p.Log(logger.Info, "reloading configuration (file changed)")

			// 重新加载配置文件
			newConf, _, err := conf.Load(p.confPath, nil, p.logger)
			if err != nil {
				p.Log(logger.Error, "%s", err)
				break outer // 加载失败，退出主循环
			}

			// 热重载配置
			err = p.reloadConf(newConf, false)
			if err != nil {
				p.Log(logger.Error, "%s", err)
				break outer // 重载失败，退出主循环
			}

		case newConf := <-p.chAPIConfigSet:
			// API请求触发的配置变更
			p.Log(logger.Info, "reloading configuration (API request)")

			// 热重载配置（通过API调用）
			err := p.reloadConf(newConf, true)
			if err != nil {
				p.Log(logger.Error, "%s", err)
				break outer // 重载失败，退出主循环
			}

		case <-interrupt:
			// 收到系统中断信号（如Ctrl+C）
			p.Log(logger.Info, "shutting down gracefully")
			break outer // 优雅关闭，退出主循环

		case <-p.ctx.Done():
			// 上下文被取消（通常由Close()函数触发）
			break outer // 退出主循环
		}
	}

	// 取消上下文，通知所有子协程开始关闭
	p.ctxCancel()

	// 关闭所有资源
	p.closeResources(nil, false)
}

// createResources负责初始化所有子系统
// 这是MediaMTX启动过程中最复杂的函数，按依赖顺序初始化各个组件
// initial参数：true表示首次启动，false表示热重载
func (p *Core) createResources(initial bool) error {
	var err error

	// 1. 初始化日志系统（所有其他组件都依赖日志）
	if p.logger == nil {
		p.logger, err = logger.New(
			logger.Level(p.conf.LogLevel), // 日志级别
			p.conf.LogDestinations,        // 日志输出目标
			p.conf.LogFile,                // 日志文件路径
			p.conf.SysLogPrefix,           // 系统日志前缀
		)
		if err != nil {
			return err
		}
	}

	// 2. 首次启动时的特殊处理
	if initial {
		// 输出启动信息
		p.Log(logger.Info, "MediaMTX %s", version)

		// 显示配置文件加载路径
		if p.confPath != "" {
			a, _ := filepath.Abs(p.confPath)
			p.Log(logger.Info, "configuration loaded from %s", a)
		} else {
			// 如果没有找到配置文件，显示搜索路径
			list := make([]string, len(defaultConfPaths))
			for i, pa := range defaultConfPaths {
				a, _ := filepath.Abs(pa)
				list[i] = a
			}

			p.Log(logger.Warn,
				"configuration file not found (looked in %s), using an empty configuration",
				strings.Join(list, ", "))
		}

		// 在Linux系统上，尝试提高文件描述符数量限制
		// 这允许支持更多的并发客户端连接
		rlimit.Raise() //nolint:errcheck

		// 设置Gin框架为生产模式，关闭调试信息
		gin.SetMode(gin.ReleaseMode)

		// 初始化外部命令池，用于执行钩子命令
		p.externalCmdPool = &externalcmd.Pool{}
		p.externalCmdPool.Initialize()
	}

	// 3. 初始化认证管理器（API和各协议服务器都需要）
	if p.authManager == nil {
		p.authManager = &auth.Manager{
			Method:             p.conf.AuthMethod,                 // 认证方法（internal/http/jwt）
			InternalUsers:      p.conf.AuthInternalUsers,          // 内部用户列表
			HTTPAddress:        p.conf.AuthHTTPAddress,            // HTTP认证服务器地址
			HTTPExclude:        p.conf.AuthHTTPExclude,            // HTTP认证排除路径
			JWTJWKS:            p.conf.AuthJWTJWKS,                // JWT密钥集
			JWTJWKSFingerprint: p.conf.AuthJWTJWKSFingerprint,     // JWT密钥集指纹
			JWTClaimKey:        p.conf.AuthJWTClaimKey,            // JWT声明键
			JWTExclude:         p.conf.AuthJWTExclude,             // JWT排除路径
			JWTInHTTPQuery:     p.conf.AuthJWTInHTTPQuery,         // JWT是否在HTTP查询参数中
			ReadTimeout:        time.Duration(p.conf.ReadTimeout), // 读取超时时间
		}
	}

	// 4. 初始化Prometheus监控指标（如果启用）
	if p.conf.Metrics &&
		p.metrics == nil {
		i := &metrics.Metrics{
			Address:        p.conf.MetricsAddress,        // 监控服务器地址
			Encryption:     p.conf.MetricsEncryption,     // 加密设置
			ServerKey:      p.conf.MetricsServerKey,      // 服务器私钥
			ServerCert:     p.conf.MetricsServerCert,     // 服务器证书
			AllowOrigin:    p.conf.MetricsAllowOrigin,    // 允许的源
			TrustedProxies: p.conf.MetricsTrustedProxies, // 受信任的代理
			ReadTimeout:    p.conf.ReadTimeout,           // 读取超时
			AuthManager:    p.authManager,                // 认证管理器
			Parent:         p,                            // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.metrics = i
	}

	// 5. 初始化性能分析（pprof，如果启用）
	if p.conf.PPROF &&
		p.pprof == nil {
		i := &pprof.PPROF{
			Address:        p.conf.PPROFAddress,        // pprof服务器地址
			Encryption:     p.conf.PPROFEncryption,     // 加密设置
			ServerKey:      p.conf.PPROFServerKey,      // 服务器私钥
			ServerCert:     p.conf.PPROFServerCert,     // 服务器证书
			AllowOrigin:    p.conf.PPROFAllowOrigin,    // 允许的源
			TrustedProxies: p.conf.PPROFTrustedProxies, // 受信任的代理
			ReadTimeout:    p.conf.ReadTimeout,         // 读取超时
			AuthManager:    p.authManager,              // 认证管理器
			Parent:         p,                          // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.pprof = i
	}

	// 6. 初始化录制文件清理器（如果有路径配置了自动删除）
	if p.recordCleaner == nil &&
		atLeastOneRecordDeleteAfter(p.conf.Paths) {
		p.recordCleaner = &recordcleaner.Cleaner{
			PathConfs: p.conf.Paths, // 路径配置
			Parent:    p,            // 父级引用
		}
		p.recordCleaner.Initialize()
	}

	// 7. 初始化回放HTTP服务器（如果启用）
	if p.conf.Playback &&
		p.playbackServer == nil {
		i := &playback.Server{
			Address:        p.conf.PlaybackAddress,        // 回放服务器地址
			Encryption:     p.conf.PlaybackEncryption,     // 加密设置
			ServerKey:      p.conf.PlaybackServerKey,      // 服务器私钥
			ServerCert:     p.conf.PlaybackServerCert,     // 服务器证书
			AllowOrigin:    p.conf.PlaybackAllowOrigin,    // 允许的源
			TrustedProxies: p.conf.PlaybackTrustedProxies, // 受信任的代理
			ReadTimeout:    p.conf.ReadTimeout,            // 读取超时
			PathConfs:      p.conf.Paths,                  // 路径配置
			AuthManager:    p.authManager,                 // 认证管理器
			Parent:         p,                             // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.playbackServer = i
	}

	// 8. 初始化路径管理器（流媒体路径的核心调度器）
	if p.pathManager == nil {
		// 计算RTP最大负载大小，考虑加密和MTU限制
		rtpMaxPayloadSize := getRTPMaxPayloadSize(p.conf.UDPMaxPayloadSize, p.conf.RTSPEncryption)

		p.pathManager = &pathManager{
			logLevel:          p.conf.LogLevel,       // 日志级别
			authManager:       p.authManager,         // 认证管理器
			rtspAddress:       p.conf.RTSPAddress,    // RTSP服务器地址
			readTimeout:       p.conf.ReadTimeout,    // 读取超时
			writeTimeout:      p.conf.WriteTimeout,   // 写入超时
			writeQueueSize:    p.conf.WriteQueueSize, // 写入队列大小
			rtpMaxPayloadSize: rtpMaxPayloadSize,     // RTP最大负载大小
			pathConfs:         p.conf.Paths,          // 路径配置
			externalCmdPool:   p.externalCmdPool,     // 外部命令池
			metrics:           p.metrics,             // 监控指标
			parent:            p,                     // 父级引用
		}
		p.pathManager.initialize()
	}

	// 9. 初始化RTSP服务器（明文，如果启用且配置允许）
	if p.conf.RTSP &&
		(p.conf.RTSPEncryption == conf.EncryptionNo ||
			p.conf.RTSPEncryption == conf.EncryptionOptional) &&
		p.rtspServer == nil {
		// 检查传输协议配置
		_, useUDP := p.conf.RTSPTransports[gortsplib.TransportUDP]
		_, useMulticast := p.conf.RTSPTransports[gortsplib.TransportUDPMulticast]

		i := &rtsp.Server{
			Address:             p.conf.RTSPAddress,         // 服务器地址
			AuthMethods:         p.conf.RTSPAuthMethods,     // 认证方法
			ReadTimeout:         p.conf.ReadTimeout,         // 读取超时
			WriteTimeout:        p.conf.WriteTimeout,        // 写入超时
			WriteQueueSize:      p.conf.WriteQueueSize,      // 写入队列大小
			UseUDP:              useUDP,                     // 是否使用UDP传输
			UseMulticast:        useMulticast,               // 是否使用多播
			RTPAddress:          p.conf.RTPAddress,          // RTP地址
			RTCPAddress:         p.conf.RTCPAddress,         // RTCP地址
			MulticastIPRange:    p.conf.MulticastIPRange,    // 多播IP范围
			MulticastRTPPort:    p.conf.MulticastRTPPort,    // 多播RTP端口
			MulticastRTCPPort:   p.conf.MulticastRTCPPort,   // 多播RTCP端口
			IsTLS:               false,                      // 不使用TLS（明文）
			ServerCert:          "",                         // 服务器证书（空）
			ServerKey:           "",                         // 服务器私钥（空）
			RTSPAddress:         p.conf.RTSPAddress,         // RTSP地址
			Transports:          p.conf.RTSPTransports,      // 传输协议
			RunOnConnect:        p.conf.RunOnConnect,        // 连接时执行的命令
			RunOnConnectRestart: p.conf.RunOnConnectRestart, // 连接命令是否重启
			RunOnDisconnect:     p.conf.RunOnDisconnect,     // 断开时执行的命令
			ExternalCmdPool:     p.externalCmdPool,          // 外部命令池
			Metrics:             p.metrics,                  // 监控指标
			PathManager:         p.pathManager,              // 路径管理器
			Parent:              p,                          // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.rtspServer = i
	}

	// 10. 初始化RTSPS服务器（加密，如果启用且配置要求）
	if p.conf.RTSP &&
		(p.conf.RTSPEncryption == conf.EncryptionStrict ||
			p.conf.RTSPEncryption == conf.EncryptionOptional) &&
		p.rtspsServer == nil {
		// 检查传输协议配置
		_, useUDP := p.conf.RTSPTransports[gortsplib.TransportUDP]
		_, useMulticast := p.conf.RTSPTransports[gortsplib.TransportUDPMulticast]

		i := &rtsp.Server{
			Address:             p.conf.RTSPSAddress,        // 加密服务器地址
			AuthMethods:         p.conf.RTSPAuthMethods,     // 认证方法
			ReadTimeout:         p.conf.ReadTimeout,         // 读取超时
			WriteTimeout:        p.conf.WriteTimeout,        // 写入超时
			WriteQueueSize:      p.conf.WriteQueueSize,      // 写入队列大小
			UseUDP:              useUDP,                     // 是否使用UDP传输
			UseMulticast:        useMulticast,               // 是否使用多播
			RTPAddress:          p.conf.SRTPAddress,         // SRTP地址
			RTCPAddress:         p.conf.SRTCPAddress,        // SRTCP地址
			MulticastIPRange:    p.conf.MulticastIPRange,    // 多播IP范围
			MulticastRTPPort:    p.conf.MulticastSRTPPort,   // 多播SRTP端口
			MulticastRTCPPort:   p.conf.MulticastSRTCPPort,  // 多播SRTCP端口
			IsTLS:               true,                       // 使用TLS（加密）
			ServerCert:          p.conf.RTSPServerCert,      // 服务器证书
			ServerKey:           p.conf.RTSPServerKey,       // 服务器私钥
			RTSPAddress:         p.conf.RTSPAddress,         // RTSP地址
			Transports:          p.conf.RTSPTransports,      // 传输协议
			RunOnConnect:        p.conf.RunOnConnect,        // 连接时执行的命令
			RunOnConnectRestart: p.conf.RunOnConnectRestart, // 连接命令是否重启
			RunOnDisconnect:     p.conf.RunOnDisconnect,     // 断开时执行的命令
			ExternalCmdPool:     p.externalCmdPool,          // 外部命令池
			Metrics:             p.metrics,                  // 监控指标
			PathManager:         p.pathManager,              // 路径管理器
			Parent:              p,                          // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.rtspsServer = i
	}

	// 11. 初始化RTMP服务器（明文，如果启用且配置允许）
	if p.conf.RTMP &&
		(p.conf.RTMPEncryption == conf.EncryptionNo ||
			p.conf.RTMPEncryption == conf.EncryptionOptional) &&
		p.rtmpServer == nil {
		i := &rtmp.Server{
			Address:             p.conf.RTMPAddress,         // RTMP服务器地址
			ReadTimeout:         p.conf.ReadTimeout,         // 读取超时
			WriteTimeout:        p.conf.WriteTimeout,        // 写入超时
			IsTLS:               false,                      // 不使用TLS（明文）
			ServerCert:          "",                         // 服务器证书（空）
			ServerKey:           "",                         // 服务器私钥（空）
			RTSPAddress:         p.conf.RTSPAddress,         // RTSP地址（用于重定向）
			RunOnConnect:        p.conf.RunOnConnect,        // 连接时执行的命令
			RunOnConnectRestart: p.conf.RunOnConnectRestart, // 连接命令是否重启
			RunOnDisconnect:     p.conf.RunOnDisconnect,     // 断开时执行的命令
			ExternalCmdPool:     p.externalCmdPool,          // 外部命令池
			Metrics:             p.metrics,                  // 监控指标
			PathManager:         p.pathManager,              // 路径管理器
			Parent:              p,                          // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.rtmpServer = i
	}

	// 12. 初始化RTMPS服务器（加密，如果启用且配置要求）
	if p.conf.RTMP &&
		(p.conf.RTMPEncryption == conf.EncryptionStrict ||
			p.conf.RTMPEncryption == conf.EncryptionOptional) &&
		p.rtmpsServer == nil {
		i := &rtmp.Server{
			Address:             p.conf.RTMPSAddress,        // RTMPS服务器地址
			ReadTimeout:         p.conf.ReadTimeout,         // 读取超时
			WriteTimeout:        p.conf.WriteTimeout,        // 写入超时
			IsTLS:               true,                       // 使用TLS（加密）
			ServerCert:          p.conf.RTMPServerCert,      // 服务器证书
			ServerKey:           p.conf.RTMPServerKey,       // 服务器私钥
			RTSPAddress:         p.conf.RTSPAddress,         // RTSP地址（用于重定向）
			RunOnConnect:        p.conf.RunOnConnect,        // 连接时执行的命令
			RunOnConnectRestart: p.conf.RunOnConnectRestart, // 连接命令是否重启
			RunOnDisconnect:     p.conf.RunOnDisconnect,     // 断开时执行的命令
			ExternalCmdPool:     p.externalCmdPool,          // 外部命令池
			Metrics:             p.metrics,                  // 监控指标
			PathManager:         p.pathManager,              // 路径管理器
			Parent:              p,                          // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.rtmpsServer = i
	}

	// 13. 初始化HLS服务器（HTTP Live Streaming，如果启用）
	if p.conf.HLS &&
		p.hlsServer == nil {
		i := &hls.Server{
			Address:         p.conf.HLSAddress,         // HLS服务器地址
			Encryption:      p.conf.HLSEncryption,      // 加密设置
			ServerKey:       p.conf.HLSServerKey,       // 服务器私钥
			ServerCert:      p.conf.HLSServerCert,      // 服务器证书
			AllowOrigin:     p.conf.HLSAllowOrigin,     // 允许的源（CORS）
			TrustedProxies:  p.conf.HLSTrustedProxies,  // 受信任的代理
			AlwaysRemux:     p.conf.HLSAlwaysRemux,     // 是否总是重新封装
			Variant:         p.conf.HLSVariant,         // HLS变体配置
			SegmentCount:    p.conf.HLSSegmentCount,    // 片段数量
			SegmentDuration: p.conf.HLSSegmentDuration, // 片段时长
			PartDuration:    p.conf.HLSPartDuration,    // 部分时长
			SegmentMaxSize:  p.conf.HLSSegmentMaxSize,  // 片段最大大小
			Directory:       p.conf.HLSDirectory,       // HLS文件目录
			ReadTimeout:     p.conf.ReadTimeout,        // 读取超时
			MuxerCloseAfter: p.conf.HLSMuxerCloseAfter, // 封装器关闭时间
			Metrics:         p.metrics,                 // 监控指标
			PathManager:     p.pathManager,             // 路径管理器
			Parent:          p,                         // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.hlsServer = i
	}

	// 14. 初始化WebRTC服务器（如果启用）
	if p.conf.WebRTC &&
		p.webRTCServer == nil {
		i := &webrtc.Server{
			Address:               p.conf.WebRTCAddress,               // WebRTC服务器地址
			Encryption:            p.conf.WebRTCEncryption,            // 加密设置
			ServerKey:             p.conf.WebRTCServerKey,             // 服务器私钥
			ServerCert:            p.conf.WebRTCServerCert,            // 服务器证书
			AllowOrigin:           p.conf.WebRTCAllowOrigin,           // 允许的源（CORS）
			TrustedProxies:        p.conf.WebRTCTrustedProxies,        // 受信任的代理
			ReadTimeout:           p.conf.ReadTimeout,                 // 读取超时
			LocalUDPAddress:       p.conf.WebRTCLocalUDPAddress,       // 本地UDP地址
			LocalTCPAddress:       p.conf.WebRTCLocalTCPAddress,       // 本地TCP地址
			IPsFromInterfaces:     p.conf.WebRTCIPsFromInterfaces,     // 是否从接口获取IP
			IPsFromInterfacesList: p.conf.WebRTCIPsFromInterfacesList, // 接口列表
			AdditionalHosts:       p.conf.WebRTCAdditionalHosts,       // 额外主机
			ICEServers:            p.conf.WebRTCICEServers2,           // ICE服务器配置
			HandshakeTimeout:      p.conf.WebRTCHandshakeTimeout,      // 握手超时
			STUNGatherTimeout:     p.conf.WebRTCSTUNGatherTimeout,     // STUN收集超时
			TrackGatherTimeout:    p.conf.WebRTCTrackGatherTimeout,    // 轨道收集超时
			ExternalCmdPool:       p.externalCmdPool,                  // 外部命令池
			Metrics:               p.metrics,                          // 监控指标
			PathManager:           p.pathManager,                      // 路径管理器
			Parent:                p,                                  // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.webRTCServer = i
	}

	// 15. 初始化SRT服务器（Secure Reliable Transport，如果启用）
	if p.conf.SRT &&
		p.srtServer == nil {
		i := &srt.Server{
			Address:             p.conf.SRTAddress,          // SRT服务器地址
			RTSPAddress:         p.conf.RTSPAddress,         // RTSP地址（用于重定向）
			ReadTimeout:         p.conf.ReadTimeout,         // 读取超时
			WriteTimeout:        p.conf.WriteTimeout,        // 写入超时
			UDPMaxPayloadSize:   p.conf.UDPMaxPayloadSize,   // UDP最大负载大小
			RunOnConnect:        p.conf.RunOnConnect,        // 连接时执行的命令
			RunOnConnectRestart: p.conf.RunOnConnectRestart, // 连接命令是否重启
			RunOnDisconnect:     p.conf.RunOnDisconnect,     // 断开时执行的命令
			ExternalCmdPool:     p.externalCmdPool,          // 外部命令池
			Metrics:             p.metrics,                  // 监控指标
			PathManager:         p.pathManager,              // 路径管理器
			Parent:              p,                          // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.srtServer = i
	}

	// 16. 初始化控制API（如果启用）
	if p.conf.API &&
		p.api == nil {
		i := &api.API{
			Address:        p.conf.APIAddress,        // API服务器地址
			Encryption:     p.conf.APIEncryption,     // 加密设置
			ServerKey:      p.conf.APIServerKey,      // 服务器私钥
			ServerCert:     p.conf.APIServerCert,     // 服务器证书
			AllowOrigin:    p.conf.APIAllowOrigin,    // 允许的源（CORS）
			TrustedProxies: p.conf.APITrustedProxies, // 受信任的代理
			ReadTimeout:    p.conf.ReadTimeout,       // 读取超时
			Conf:           p.conf,                   // 配置对象
			AuthManager:    p.authManager,            // 认证管理器
			PathManager:    p.pathManager,            // 路径管理器
			RTSPServer:     p.rtspServer,             // RTSP服务器引用
			RTSPSServer:    p.rtspsServer,            // RTSPS服务器引用
			RTMPServer:     p.rtmpServer,             // RTMP服务器引用
			RTMPSServer:    p.rtmpsServer,            // RTMPS服务器引用
			HLSServer:      p.hlsServer,              // HLS服务器引用
			WebRTCServer:   p.webRTCServer,           // WebRTC服务器引用
			SRTServer:      p.srtServer,              // SRT服务器引用
			Parent:         p,                        // 父级引用
		}
		err = i.Initialize()
		if err != nil {
			return err
		}
		p.api = i
	}

	// 17. 初始化配置文件监听器（仅在首次启动且有配置文件时）
	if initial && p.confPath != "" {
		cf := &confwatcher.ConfWatcher{FilePath: p.confPath}
		err = cf.Initialize()
		if err != nil {
			return err
		}
		p.confWatcher = cf
	}

	return nil
}

// closeResources负责关闭所有子系统
// 支持细粒度的热重载，只关闭需要重建的资源
// newConf: 新配置，nil表示完全关闭
// calledByAPI: 是否由API调用，避免循环调用
func (p *Core) closeResources(newConf *conf.Conf, calledByAPI bool) {
	// 判断是否需要关闭日志系统
	// 当配置变更影响日志设置时，需要重新初始化
	closeLogger := newConf == nil ||
		newConf.LogLevel != p.conf.LogLevel ||
		!reflect.DeepEqual(newConf.LogDestinations, p.conf.LogDestinations) ||
		newConf.LogFile != p.conf.LogFile ||
		newConf.SysLogPrefix != p.conf.SysLogPrefix

	// 判断是否需要关闭认证管理器
	// 当认证相关配置变更时，需要重新初始化
	closeAuthManager := newConf == nil ||
		newConf.AuthMethod != p.conf.AuthMethod ||
		newConf.AuthHTTPAddress != p.conf.AuthHTTPAddress ||
		!reflect.DeepEqual(newConf.AuthHTTPExclude, p.conf.AuthHTTPExclude) ||
		newConf.AuthJWTJWKS != p.conf.AuthJWTJWKS ||
		newConf.AuthJWTJWKSFingerprint != p.conf.AuthJWTJWKSFingerprint ||
		newConf.AuthJWTClaimKey != p.conf.AuthJWTClaimKey ||
		!reflect.DeepEqual(newConf.AuthJWTExclude, p.conf.AuthJWTExclude) ||
		newConf.AuthJWTInHTTPQuery != p.conf.AuthJWTInHTTPQuery ||
		newConf.ReadTimeout != p.conf.ReadTimeout

	// 如果认证管理器不需要关闭，但内部用户列表变更，则重新加载
	if !closeAuthManager && !reflect.DeepEqual(newConf.AuthInternalUsers, p.conf.AuthInternalUsers) {
		p.authManager.ReloadInternalUsers(newConf.AuthInternalUsers)
	}

	// 判断是否需要关闭监控指标
	// 当监控配置变更或依赖的组件变更时，需要重新初始化
	closeMetrics := newConf == nil ||
		newConf.Metrics != p.conf.Metrics ||
		newConf.MetricsAddress != p.conf.MetricsAddress ||
		newConf.MetricsEncryption != p.conf.MetricsEncryption ||
		newConf.MetricsServerKey != p.conf.MetricsServerKey ||
		newConf.MetricsServerCert != p.conf.MetricsServerCert ||
		newConf.MetricsAllowOrigin != p.conf.MetricsAllowOrigin ||
		!reflect.DeepEqual(newConf.MetricsTrustedProxies, p.conf.MetricsTrustedProxies) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		closeAuthManager ||
		closeLogger

	// 判断是否需要关闭性能分析
	// 当pprof配置变更或依赖的组件变更时，需要重新初始化
	closePPROF := newConf == nil ||
		newConf.PPROF != p.conf.PPROF ||
		newConf.PPROFAddress != p.conf.PPROFAddress ||
		newConf.PPROFEncryption != p.conf.PPROFEncryption ||
		newConf.PPROFServerKey != p.conf.PPROFServerKey ||
		newConf.PPROFServerCert != p.conf.PPROFServerCert ||
		newConf.PPROFAllowOrigin != p.conf.PPROFAllowOrigin ||
		!reflect.DeepEqual(newConf.PPROFTrustedProxies, p.conf.PPROFTrustedProxies) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		closeAuthManager ||
		closeLogger

	// 判断是否需要关闭录制清理器
	// 当路径配置变更影响录制删除设置时，需要重新初始化
	closeRecorderCleaner := newConf == nil ||
		atLeastOneRecordDeleteAfter(newConf.Paths) != atLeastOneRecordDeleteAfter(p.conf.Paths) ||
		closeLogger

	// 如果录制清理器不需要关闭，但路径配置变更，则重新加载
	if !closeRecorderCleaner && p.recordCleaner != nil && !reflect.DeepEqual(newConf.Paths, p.conf.Paths) {
		p.recordCleaner.ReloadPathConfs(newConf.Paths)
	}

	// 判断是否需要关闭回放服务器
	// 当回放配置变更或依赖的组件变更时，需要重新初始化
	closePlaybackServer := newConf == nil ||
		newConf.Playback != p.conf.Playback ||
		newConf.PlaybackAddress != p.conf.PlaybackAddress ||
		newConf.PlaybackEncryption != p.conf.PlaybackEncryption ||
		newConf.PlaybackServerKey != p.conf.PlaybackServerKey ||
		newConf.PlaybackServerCert != p.conf.PlaybackServerCert ||
		newConf.PlaybackAllowOrigin != p.conf.PlaybackAllowOrigin ||
		!reflect.DeepEqual(newConf.PlaybackTrustedProxies, p.conf.PlaybackTrustedProxies) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		closeAuthManager ||
		closeLogger

	// 如果回放服务器不需要关闭，但路径配置变更，则重新加载
	if !closePlaybackServer && p.playbackServer != nil && !reflect.DeepEqual(newConf.Paths, p.conf.Paths) {
		p.playbackServer.ReloadPathConfs(newConf.Paths)
	}

	// 判断是否需要关闭路径管理器
	// 当路径相关配置变更或依赖的组件变更时，需要重新初始化
	closePathManager := newConf == nil ||
		newConf.LogLevel != p.conf.LogLevel ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.WriteQueueSize != p.conf.WriteQueueSize ||
		newConf.UDPMaxPayloadSize != p.conf.UDPMaxPayloadSize ||
		newConf.RTSPEncryption != p.conf.RTSPEncryption ||
		closeMetrics ||
		closeAuthManager ||
		closeLogger

	// 如果路径管理器不需要关闭，但路径配置变更，则重新加载
	if !closePathManager && !reflect.DeepEqual(newConf.Paths, p.conf.Paths) {
		p.pathManager.ReloadPathConfs(newConf.Paths)
	}

	// 判断是否需要关闭RTSP服务器（明文）
	// 当RTSP相关配置变更或依赖的组件变更时，需要重新初始化
	closeRTSPServer := newConf == nil ||
		newConf.RTSP != p.conf.RTSP ||
		newConf.RTSPEncryption != p.conf.RTSPEncryption ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		!reflect.DeepEqual(newConf.RTSPAuthMethods, p.conf.RTSPAuthMethods) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.WriteQueueSize != p.conf.WriteQueueSize ||
		newConf.RTPAddress != p.conf.RTPAddress ||
		newConf.RTCPAddress != p.conf.RTCPAddress ||
		newConf.MulticastIPRange != p.conf.MulticastIPRange ||
		newConf.MulticastRTPPort != p.conf.MulticastRTPPort ||
		newConf.MulticastRTCPPort != p.conf.MulticastRTCPPort ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		!reflect.DeepEqual(newConf.RTSPTransports, p.conf.RTSPTransports) ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		newConf.RunOnDisconnect != p.conf.RunOnDisconnect ||
		closeMetrics ||
		closePathManager ||
		closeLogger

	// 判断是否需要关闭RTSPS服务器（加密）
	// 当RTSPS相关配置变更或依赖的组件变更时，需要重新初始化
	closeRTSPSServer := newConf == nil ||
		newConf.RTSP != p.conf.RTSP ||
		newConf.RTSPEncryption != p.conf.RTSPEncryption ||
		newConf.RTSPSAddress != p.conf.RTSPSAddress ||
		!reflect.DeepEqual(newConf.RTSPAuthMethods, p.conf.RTSPAuthMethods) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.WriteQueueSize != p.conf.WriteQueueSize ||
		newConf.RTSPServerCert != p.conf.RTSPServerCert ||
		newConf.RTSPServerKey != p.conf.RTSPServerKey ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		!reflect.DeepEqual(newConf.RTSPTransports, p.conf.RTSPTransports) ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		newConf.RunOnDisconnect != p.conf.RunOnDisconnect ||
		closeMetrics ||
		closePathManager ||
		closeLogger

	// 判断是否需要关闭RTMP服务器（明文）
	// 当RTMP相关配置变更或依赖的组件变更时，需要重新初始化
	closeRTMPServer := newConf == nil ||
		newConf.RTMP != p.conf.RTMP ||
		newConf.RTMPEncryption != p.conf.RTMPEncryption ||
		newConf.RTMPAddress != p.conf.RTMPAddress ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		newConf.RunOnDisconnect != p.conf.RunOnDisconnect ||
		closeMetrics ||
		closePathManager ||
		closeLogger

	// 判断是否需要关闭RTMPS服务器（加密）
	// 当RTMPS相关配置变更或依赖的组件变更时，需要重新初始化
	closeRTMPSServer := newConf == nil ||
		newConf.RTMP != p.conf.RTMP ||
		newConf.RTMPEncryption != p.conf.RTMPEncryption ||
		newConf.RTMPSAddress != p.conf.RTMPSAddress ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.RTMPServerCert != p.conf.RTMPServerCert ||
		newConf.RTMPServerKey != p.conf.RTMPServerKey ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		newConf.RunOnDisconnect != p.conf.RunOnDisconnect ||
		closeMetrics ||
		closePathManager ||
		closeLogger

	// 判断是否需要关闭HLS服务器
	// 当HLS相关配置变更或依赖的组件变更时，需要重新初始化
	closeHLSServer := newConf == nil ||
		newConf.HLS != p.conf.HLS ||
		newConf.HLSAddress != p.conf.HLSAddress ||
		newConf.HLSEncryption != p.conf.HLSEncryption ||
		newConf.HLSServerKey != p.conf.HLSServerKey ||
		newConf.HLSServerCert != p.conf.HLSServerCert ||
		newConf.HLSAllowOrigin != p.conf.HLSAllowOrigin ||
		!reflect.DeepEqual(newConf.HLSTrustedProxies, p.conf.HLSTrustedProxies) ||
		newConf.HLSAlwaysRemux != p.conf.HLSAlwaysRemux ||
		newConf.HLSVariant != p.conf.HLSVariant ||
		newConf.HLSSegmentCount != p.conf.HLSSegmentCount ||
		newConf.HLSSegmentDuration != p.conf.HLSSegmentDuration ||
		newConf.HLSPartDuration != p.conf.HLSPartDuration ||
		newConf.HLSSegmentMaxSize != p.conf.HLSSegmentMaxSize ||
		newConf.HLSDirectory != p.conf.HLSDirectory ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.HLSMuxerCloseAfter != p.conf.HLSMuxerCloseAfter ||
		closePathManager ||
		closeMetrics ||
		closeLogger

	// 判断是否需要关闭WebRTC服务器
	// 当WebRTC相关配置变更或依赖的组件变更时，需要重新初始化
	closeWebRTCServer := newConf == nil ||
		newConf.WebRTC != p.conf.WebRTC ||
		newConf.WebRTCAddress != p.conf.WebRTCAddress ||
		newConf.WebRTCEncryption != p.conf.WebRTCEncryption ||
		newConf.WebRTCServerKey != p.conf.WebRTCServerKey ||
		newConf.WebRTCServerCert != p.conf.WebRTCServerCert ||
		newConf.WebRTCAllowOrigin != p.conf.WebRTCAllowOrigin ||
		!reflect.DeepEqual(newConf.WebRTCTrustedProxies, p.conf.WebRTCTrustedProxies) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WebRTCLocalUDPAddress != p.conf.WebRTCLocalUDPAddress ||
		newConf.WebRTCLocalTCPAddress != p.conf.WebRTCLocalTCPAddress ||
		newConf.WebRTCIPsFromInterfaces != p.conf.WebRTCIPsFromInterfaces ||
		!reflect.DeepEqual(newConf.WebRTCIPsFromInterfacesList, p.conf.WebRTCIPsFromInterfacesList) ||
		!reflect.DeepEqual(newConf.WebRTCAdditionalHosts, p.conf.WebRTCAdditionalHosts) ||
		!reflect.DeepEqual(newConf.WebRTCICEServers2, p.conf.WebRTCICEServers2) ||
		newConf.WebRTCHandshakeTimeout != p.conf.WebRTCHandshakeTimeout ||
		newConf.WebRTCSTUNGatherTimeout != p.conf.WebRTCSTUNGatherTimeout ||
		newConf.WebRTCTrackGatherTimeout != p.conf.WebRTCTrackGatherTimeout ||
		closeMetrics ||
		closePathManager ||
		closeLogger

	// 判断是否需要关闭SRT服务器
	// 当SRT相关配置变更或依赖的组件变更时，需要重新初始化
	closeSRTServer := newConf == nil ||
		newConf.SRT != p.conf.SRT ||
		newConf.SRTAddress != p.conf.SRTAddress ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.UDPMaxPayloadSize != p.conf.UDPMaxPayloadSize ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		newConf.RunOnDisconnect != p.conf.RunOnDisconnect ||
		closePathManager ||
		closeLogger

	// 判断是否需要关闭API服务器
	// 当API相关配置变更或依赖的组件变更时，需要重新初始化
	closeAPI := newConf == nil ||
		newConf.API != p.conf.API ||
		newConf.APIAddress != p.conf.APIAddress ||
		newConf.APIEncryption != p.conf.APIEncryption ||
		newConf.APIServerKey != p.conf.APIServerKey ||
		newConf.APIServerCert != p.conf.APIServerCert ||
		newConf.APIAllowOrigin != p.conf.APIAllowOrigin ||
		!reflect.DeepEqual(newConf.APITrustedProxies, p.conf.APITrustedProxies) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		closeAuthManager ||
		closePathManager ||
		closeRTSPServer ||
		closeRTSPSServer ||
		closeRTMPServer ||
		closeHLSServer ||
		closeWebRTCServer ||
		closeSRTServer ||
		closeLogger

	// 关闭配置文件监听器（仅在完全关闭时）
	if newConf == nil && p.confWatcher != nil {
		p.confWatcher.Close()
		p.confWatcher = nil
	}

	// 实际关闭操作 - 按依赖关系的反序关闭
	// 1. 关闭API服务器（依赖其他所有服务器）
	if p.api != nil {
		if closeAPI {
			p.api.Close()
			p.api = nil
		} else if !calledByAPI { // 避免循环调用
			p.api.ReloadConf(newConf)
		}
	}

	// 2. 关闭SRT服务器
	if closeSRTServer && p.srtServer != nil {
		p.srtServer.Close()
		p.srtServer = nil
	}

	// 3. 关闭WebRTC服务器
	if closeWebRTCServer && p.webRTCServer != nil {
		p.webRTCServer.Close()
		p.webRTCServer = nil
	}

	// 4. 关闭HLS服务器
	if closeHLSServer && p.hlsServer != nil {
		p.hlsServer.Close()
		p.hlsServer = nil
	}

	// 5. 关闭RTMPS服务器（加密）
	if closeRTMPSServer && p.rtmpsServer != nil {
		p.rtmpsServer.Close()
		p.rtmpsServer = nil
	}

	// 6. 关闭RTMP服务器（明文）
	if closeRTMPServer && p.rtmpServer != nil {
		p.rtmpServer.Close()
		p.rtmpServer = nil
	}

	// 7. 关闭RTSPS服务器（加密）
	if closeRTSPSServer && p.rtspsServer != nil {
		p.rtspsServer.Close()
		p.rtspsServer = nil
	}

	// 8. 关闭RTSP服务器（明文）
	if closeRTSPServer && p.rtspServer != nil {
		p.rtspServer.Close()
		p.rtspServer = nil
	}

	// 9. 关闭路径管理器（依赖所有协议服务器）
	if closePathManager && p.pathManager != nil {
		p.pathManager.close()
		p.pathManager = nil
	}

	// 10. 关闭回放服务器
	if closePlaybackServer && p.playbackServer != nil {
		p.playbackServer.Close()
		p.playbackServer = nil
	}

	// 11. 关闭录制清理器
	if closeRecorderCleaner && p.recordCleaner != nil {
		p.recordCleaner.Close()
		p.recordCleaner = nil
	}

	// 12. 关闭性能分析
	if closePPROF && p.pprof != nil {
		p.pprof.Close()
		p.pprof = nil
	}

	// 13. 关闭监控指标
	if closeMetrics && p.metrics != nil {
		p.metrics.Close()
		p.metrics = nil
	}

	// 14. 关闭认证管理器
	if closeAuthManager && p.authManager != nil {
		p.authManager = nil
	}

	// 15. 关闭外部命令池（等待所有钩子命令完成）
	if newConf == nil && p.externalCmdPool != nil {
		p.Log(logger.Info, "waiting for running hooks")
		p.externalCmdPool.Close()
	}

	// 16. 关闭日志系统（最后关闭）
	if closeLogger && p.logger != nil {
		p.logger.Close()
		p.logger = nil
	}
}

// reloadConf执行配置热重载
// 先关闭需要重建的资源，再用新配置重建
// newConf: 新配置对象
// calledByAPI: 是否由API调用，用于避免循环调用
func (p *Core) reloadConf(newConf *conf.Conf, calledByAPI bool) error {
	// 关闭需要重建的资源
	p.closeResources(newConf, calledByAPI)

	// 更新当前配置
	p.conf = newConf

	// 用新配置重建资源
	return p.createResources(false)
}

// APIConfigSet是API调用的配置变更接口
// 通过通道异步通知主循环进行配置热重载
// conf: 新的配置对象
func (p *Core) APIConfigSet(conf *conf.Conf) {
	select {
	case p.chAPIConfigSet <- conf:
		// 成功发送配置变更请求
	case <-p.ctx.Done():
		// 上下文已取消，忽略配置变更
	}
}
