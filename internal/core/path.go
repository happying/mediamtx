// Package core 是MediaMTX的核心包，负责管理流媒体路径
// 路径(Path)是MediaMTX中的核心概念，代表一个流媒体通道
// 每个路径可以有一个发布者(Publisher)和多个读取者(Reader)
package core

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/description"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recorder"
	"github.com/bluenviron/mediamtx/internal/staticsources"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// emptyTimer 创建一个立即过期的定时器
// 用于初始化那些暂时不需要计时的定时器
func emptyTimer() *time.Timer {
	t := time.NewTimer(0)
	<-t.C
	return t
}

// pathParent 定义了路径的父接口
// 路径管理器(pathManager)实现了这个接口，用于管理路径的生命周期
type pathParent interface {
	logger.Writer                                                           // 日志写入接口
	pathReady(*path)                                                        // 路径准备就绪时的回调
	pathNotReady(*path)                                                     // 路径不可用时的回调
	closePath(*path)                                                        // 关闭路径
	AddReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error) // 添加读取者
}

// pathOnDemandState 定义了按需启动的状态枚举
// 用于管理静态源和发布者的按需启动状态
type pathOnDemandState int

const (
	pathOnDemandStateInitial      pathOnDemandState = iota // 初始状态
	pathOnDemandStateWaitingReady                          // 等待就绪状态
	pathOnDemandStateReady                                 // 就绪状态
	pathOnDemandStateClosing                               // 关闭中状态
)

// pathAPIPathsListRes API路径列表响应结构
type pathAPIPathsListRes struct {
	data  *defs.APIPathList // API路径列表数据
	paths map[string]*path  // 路径映射表
}

// pathAPIPathsListReq API路径列表请求结构
type pathAPIPathsListReq struct {
	res chan pathAPIPathsListRes // 响应通道
}

// pathAPIPathsGetRes API获取单个路径响应结构
type pathAPIPathsGetRes struct {
	path *path         // 路径对象
	data *defs.APIPath // API路径数据
	err  error         // 错误信息
}

// pathAPIPathsGetReq API获取单个路径请求结构
type pathAPIPathsGetReq struct {
	name string                  // 路径名称
	res  chan pathAPIPathsGetRes // 响应通道
}

// path 是MediaMTX中的核心结构体，代表一个流媒体路径
// 每个路径管理一个流媒体源和多个读取者
type path struct {
	// 基础配置字段
	parentCtx         context.Context   // 父级上下文
	logLevel          conf.LogLevel     // 日志级别
	rtspAddress       string            // RTSP服务器地址
	readTimeout       conf.Duration     // 读取超时时间
	writeTimeout      conf.Duration     // 写入超时时间
	writeQueueSize    int               // 写入队列大小
	rtpMaxPayloadSize int               // RTP最大负载大小
	conf              *conf.Path        // 路径配置
	name              string            // 路径名称
	matches           []string          // 正则表达式匹配结果
	wg                *sync.WaitGroup   // 等待组，用于同步
	externalCmdPool   *externalcmd.Pool // 外部命令池
	parent            pathParent        // 父级对象(路径管理器)

	// 运行时状态字段
	ctx                     context.Context          // 路径上下文
	ctxCancel               func()                   // 取消函数
	confMutex               sync.RWMutex             // 配置读写锁
	source                  defs.Source              // 流媒体源
	publisherQuery          string                   // 发布者查询字符串
	stream                  *stream.Stream           // 流对象
	recorder                *recorder.Recorder       // 录制器
	readyTime               time.Time                // 就绪时间
	onUnDemandHook          func(string)             // 按需停止钩子
	onNotReadyHook          func()                   // 不可用钩子
	readers                 map[defs.Reader]struct{} // 读取者集合
	describeRequestsOnHold  []defs.PathDescribeReq   // 等待的描述请求
	readerAddRequestsOnHold []defs.PathAddReaderReq  // 等待的添加读取者请求

	// 按需启动相关字段
	onDemandStaticSourceState      pathOnDemandState // 静态源按需状态
	onDemandStaticSourceReadyTimer *time.Timer       // 静态源就绪定时器
	onDemandStaticSourceCloseTimer *time.Timer       // 静态源关闭定时器
	onDemandPublisherState         pathOnDemandState // 发布者按需状态
	onDemandPublisherReadyTimer    *time.Timer       // 发布者就绪定时器
	onDemandPublisherCloseTimer    *time.Timer       // 发布者关闭定时器

	// 输入通道 - 用于接收各种请求
	chReloadConf              chan *conf.Path                          // 重新加载配置
	chStaticSourceSetReady    chan defs.PathSourceStaticSetReadyReq    // 静态源设置就绪
	chStaticSourceSetNotReady chan defs.PathSourceStaticSetNotReadyReq // 静态源设置不可用
	chDescribe                chan defs.PathDescribeReq                // 描述请求
	chAddPublisher            chan defs.PathAddPublisherReq            // 添加发布者
	chRemovePublisher         chan defs.PathRemovePublisherReq         // 移除发布者
	chStartPublisher          chan defs.PathStartPublisherReq          // 启动发布者
	chStopPublisher           chan defs.PathStopPublisherReq           // 停止发布者
	chAddReader               chan defs.PathAddReaderReq               // 添加读取者
	chRemoveReader            chan defs.PathRemoveReaderReq            // 移除读取者
	chAPIPathsGet             chan pathAPIPathsGetReq                  // API获取路径

	// 输出通道 - 用于通知完成
	done chan struct{} // 完成信号通道
}

// initialize 初始化路径对象
// 设置上下文、初始化各种通道、启动主循环协程
func (pa *path) initialize() {
	// 创建可取消的上下文
	ctx, ctxCancel := context.WithCancel(pa.parentCtx)

	pa.ctx = ctx
	pa.ctxCancel = ctxCancel
	pa.readers = make(map[defs.Reader]struct{}) // 初始化读取者映射表

	// 初始化各种定时器为空定时器
	pa.onDemandStaticSourceReadyTimer = emptyTimer()
	pa.onDemandStaticSourceCloseTimer = emptyTimer()
	pa.onDemandPublisherReadyTimer = emptyTimer()
	pa.onDemandPublisherCloseTimer = emptyTimer()

	// 初始化各种通道
	pa.chReloadConf = make(chan *conf.Path)
	pa.chStaticSourceSetReady = make(chan defs.PathSourceStaticSetReadyReq)
	pa.chStaticSourceSetNotReady = make(chan defs.PathSourceStaticSetNotReadyReq)
	pa.chDescribe = make(chan defs.PathDescribeReq)
	pa.chAddPublisher = make(chan defs.PathAddPublisherReq)
	pa.chRemovePublisher = make(chan defs.PathRemovePublisherReq)
	pa.chStartPublisher = make(chan defs.PathStartPublisherReq)
	pa.chStopPublisher = make(chan defs.PathStopPublisherReq)
	pa.chAddReader = make(chan defs.PathAddReaderReq)
	pa.chRemoveReader = make(chan defs.PathRemoveReaderReq)
	pa.chAPIPathsGet = make(chan pathAPIPathsGetReq)
	pa.done = make(chan struct{})

	pa.Log(logger.Debug, "created")

	// 启动主循环协程
	pa.wg.Add(1)
	go pa.run()
}

// close 关闭路径
// 取消上下文，触发主循环退出
func (pa *path) close() {
	pa.ctxCancel()
}

// wait 等待路径完全关闭
// 阻塞直到主循环协程结束
func (pa *path) wait() {
	<-pa.done
}

// Log 实现logger.Writer接口
// 为日志添加路径前缀，便于调试和追踪
func (pa *path) Log(level logger.Level, format string, args ...interface{}) {
	pa.parent.Log(level, "[path "+pa.name+"] "+format, args...)
}

// Name 返回路径名称
func (pa *path) Name() string {
	return pa.name
}

// isReady 检查路径是否准备就绪
// 当stream不为nil时表示路径已就绪
func (pa *path) isReady() bool {
	return pa.stream != nil
}

// run 是路径的主循环协程
// 负责初始化源、处理各种请求、管理路径生命周期
func (pa *path) run() {
	defer close(pa.done) // 确保协程结束时关闭done通道
	defer pa.wg.Done()   // 通知等待组协程已结束

	// 根据配置初始化流媒体源
	if pa.conf.Source == "redirect" {
		// 重定向源：将请求重定向到其他地址
		pa.source = &sourceRedirect{}
	} else if pa.conf.HasStaticSource() {
		// 静态源：从固定的流媒体源获取数据
		pa.source = &staticsources.Handler{
			Conf:              pa.conf,
			LogLevel:          pa.logLevel,
			ReadTimeout:       pa.readTimeout,
			WriteTimeout:      pa.writeTimeout,
			WriteQueueSize:    pa.writeQueueSize,
			RTPMaxPayloadSize: pa.rtpMaxPayloadSize,
			Matches:           pa.matches,
			PathManager:       pa.parent,
			Parent:            pa,
		}
		pa.source.(*staticsources.Handler).Initialize()

		// 如果不是按需启动，则立即启动静态源
		if !pa.conf.SourceOnDemand {
			pa.source.(*staticsources.Handler).Start(false, "")
		}
	}

	// 执行初始化钩子
	onUnInitHook := hooks.OnInit(hooks.OnInitParams{
		Logger:          pa,
		ExternalCmdPool: pa.externalCmdPool,
		Conf:            pa.conf,
		ExternalCmdEnv:  pa.ExternalCmdEnv(),
	})

	// 运行主循环
	err := pa.runInner()

	// 在销毁上下文之前调用父级关闭路径
	pa.parent.closePath(pa)

	pa.ctxCancel()

	// 停止所有定时器
	pa.onDemandStaticSourceReadyTimer.Stop()
	pa.onDemandStaticSourceCloseTimer.Stop()
	pa.onDemandPublisherReadyTimer.Stop()
	pa.onDemandPublisherCloseTimer.Stop()

	// 执行清理钩子
	onUnInitHook()

	// 处理等待中的描述请求，返回终止错误
	for _, req := range pa.describeRequestsOnHold {
		req.Res <- defs.PathDescribeRes{Err: fmt.Errorf("terminated")}
	}

	// 处理等待中的读取者添加请求，返回终止错误
	for _, req := range pa.readerAddRequestsOnHold {
		req.Res <- defs.PathAddReaderRes{Err: fmt.Errorf("terminated")}
	}

	// 如果流存在，设置为不可用状态
	if pa.stream != nil {
		pa.setNotReady()
	}

	// 关闭源
	if pa.source != nil {
		if source, ok := pa.source.(*staticsources.Handler); ok {
			// 静态源处理器
			if !pa.conf.SourceOnDemand || pa.onDemandStaticSourceState != pathOnDemandStateInitial {
				source.Close("path is closing")
			}
		} else if source, ok := pa.source.(defs.Publisher); ok {
			// 发布者
			source.Close()
		}
	}

	// 执行按需停止钩子
	if pa.onUnDemandHook != nil {
		pa.onUnDemandHook("path destroyed")
	}

	pa.Log(logger.Debug, "destroyed: %v", err)
}

// runInner 是路径的核心事件循环
// 通过select语句监听各种通道，处理不同类型的请求和事件
func (pa *path) runInner() error {
	for {
		select {
		// 按需静态源相关事件
		case <-pa.onDemandStaticSourceReadyTimer.C:
			// 静态源就绪超时：等待静态源启动超时
			pa.doOnDemandStaticSourceReadyTimer()

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		case <-pa.onDemandStaticSourceCloseTimer.C:
			// 静态源关闭超时：按需静态源关闭超时
			pa.doOnDemandStaticSourceCloseTimer()

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		// 按需发布者相关事件
		case <-pa.onDemandPublisherReadyTimer.C:
			// 发布者就绪超时：等待发布者启动超时
			pa.doOnDemandPublisherReadyTimer()

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		case <-pa.onDemandPublisherCloseTimer.C:
			// 发布者关闭超时：按需发布者关闭超时
			pa.doOnDemandPublisherCloseTimer()

		// 配置管理事件
		case newConf := <-pa.chReloadConf:
			// 重新加载配置：热重载路径配置
			pa.doReloadConf(newConf)

		// 静态源状态管理事件
		case req := <-pa.chStaticSourceSetReady:
			// 静态源设置就绪：静态源成功启动并准备就绪
			pa.doSourceStaticSetReady(req)

		case req := <-pa.chStaticSourceSetNotReady:
			// 静态源设置不可用：静态源出现错误或停止
			pa.doSourceStaticSetNotReady(req)

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		// 流媒体协议事件
		case req := <-pa.chDescribe:
			// 描述请求：客户端请求获取流的描述信息
			pa.doDescribe(req)

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		// 发布者管理事件
		case req := <-pa.chAddPublisher:
			// 添加发布者：新的发布者请求连接到路径
			pa.doAddPublisher(req)

		case req := <-pa.chRemovePublisher:
			// 移除发布者：发布者断开连接
			pa.doRemovePublisher(req)

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		case req := <-pa.chStartPublisher:
			// 启动发布者：发布者开始发布流
			pa.doStartPublisher(req)

		case req := <-pa.chStopPublisher:
			// 停止发布者：发布者停止发布流
			pa.doStopPublisher(req)

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		// 读取者管理事件
		case req := <-pa.chAddReader:
			// 添加读取者：新的读取者请求连接到路径
			pa.doAddReader(req)

			if pa.shouldClose() {
				return fmt.Errorf("not in use")
			}

		case req := <-pa.chRemoveReader:
			// 移除读取者：读取者断开连接
			pa.doRemoveReader(req)

		// API事件
		case req := <-pa.chAPIPathsGet:
			// API获取路径信息：通过API获取路径状态
			pa.doAPIPathsGet(req)

		// 上下文取消事件
		case <-pa.ctx.Done():
			// 上下文被取消：路径被要求终止
			return fmt.Errorf("terminated")
		}
	}
}

// doOnDemandStaticSourceReadyTimer 处理静态源就绪超时
// 当按需静态源在指定时间内未能启动时调用
func (pa *path) doOnDemandStaticSourceReadyTimer() {
	// 向所有等待的描述请求返回超时错误
	for _, req := range pa.describeRequestsOnHold {
		req.Res <- defs.PathDescribeRes{Err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
	}
	pa.describeRequestsOnHold = nil

	// 向所有等待的读取者添加请求返回超时错误
	for _, req := range pa.readerAddRequestsOnHold {
		req.Res <- defs.PathAddReaderRes{Err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
	}
	pa.readerAddRequestsOnHold = nil

	// 停止按需静态源
	pa.onDemandStaticSourceStop("timed out")
}

// doOnDemandStaticSourceCloseTimer 处理静态源关闭超时
// 当按需静态源在无人使用时关闭超时
func (pa *path) doOnDemandStaticSourceCloseTimer() {
	pa.setNotReady()
	pa.onDemandStaticSourceStop("not needed by anyone")
}

// doOnDemandPublisherReadyTimer 处理发布者就绪超时
// 当按需发布者在指定时间内未能启动时调用
func (pa *path) doOnDemandPublisherReadyTimer() {
	// 向所有等待的描述请求返回超时错误
	for _, req := range pa.describeRequestsOnHold {
		req.Res <- defs.PathDescribeRes{Err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
	}
	pa.describeRequestsOnHold = nil

	// 向所有等待的读取者添加请求返回超时错误
	for _, req := range pa.readerAddRequestsOnHold {
		req.Res <- defs.PathAddReaderRes{Err: fmt.Errorf("source of path '%s' has timed out", pa.name)}
	}
	pa.readerAddRequestsOnHold = nil

	// 停止按需发布者
	pa.onDemandPublisherStop("timed out")
}

// doOnDemandPublisherCloseTimer 处理发布者关闭超时
// 当按需发布者在无人使用时关闭超时
func (pa *path) doOnDemandPublisherCloseTimer() {
	pa.onDemandPublisherStop("not needed by anyone")
}

// doReloadConf 处理配置重载
// 支持热重载路径配置，无需重启服务
func (pa *path) doReloadConf(newConf *conf.Path) {
	// 更新配置（使用写锁保护）
	pa.confMutex.Lock()
	pa.conf = newConf
	pa.confMutex.Unlock()

	// 如果配置了静态源，重新加载静态源配置
	if pa.conf.HasStaticSource() {
		pa.source.(*staticsources.Handler).ReloadConf(newConf)
	}

	// 处理录制配置变更
	if pa.conf.Record {
		// 如果启用了录制但录制器不存在，启动录制
		if pa.stream != nil && pa.recorder == nil {
			pa.startRecording()
		}
	} else if pa.recorder != nil {
		// 如果禁用了录制但录制器存在，停止录制
		pa.recorder.Close()
		pa.recorder = nil
	}
}

// doSourceStaticSetReady 处理静态源设置就绪
// 当静态源成功启动并准备就绪时调用
func (pa *path) doSourceStaticSetReady(req defs.PathSourceStaticSetReadyReq) {
	// 设置路径为就绪状态
	err := pa.setReady(req.Desc, req.GenerateRTPPackets)
	if err != nil {
		req.Res <- defs.PathSourceStaticSetReadyRes{Err: err}
		return
	}

	// 如果是按需静态源，处理按需逻辑
	if pa.conf.HasOnDemandStaticSource() {
		// 停止就绪定时器
		pa.onDemandStaticSourceReadyTimer.Stop()
		pa.onDemandStaticSourceReadyTimer = emptyTimer()
		// 安排关闭定时器
		pa.onDemandStaticSourceScheduleClose()
	}

	// 处理等待中的请求
	pa.consumeOnHoldRequests()

	// 返回成功响应
	req.Res <- defs.PathSourceStaticSetReadyRes{Stream: pa.stream}
}

// doSourceStaticSetNotReady 处理静态源设置不可用
// 当静态源出现错误或停止时调用
func (pa *path) doSourceStaticSetNotReady(req defs.PathSourceStaticSetNotReadyReq) {
	// 设置路径为不可用状态
	pa.setNotReady()

	// 在调用onDemandStaticSourceStop之前发送响应
	// 避免由于staticsources.Handler.stop()导致的死锁
	close(req.Res)

	// 如果是按需静态源且不在初始状态，停止按需逻辑
	if pa.conf.HasOnDemandStaticSource() && pa.onDemandStaticSourceState != pathOnDemandStateInitial {
		pa.onDemandStaticSourceStop("an error occurred")
	}
}

// doDescribe 处理描述请求
// 客户端通过RTSP DESCRIBE等方法请求获取流的描述信息
func (pa *path) doDescribe(req defs.PathDescribeReq) {
	// 处理重定向源
	if _, ok := pa.source.(*sourceRedirect); ok {
		req.Res <- defs.PathDescribeRes{
			Redirect: pa.conf.SourceRedirect,
		}
		return
	}

	// 如果流已就绪，直接返回流信息
	if pa.stream != nil {
		req.Res <- defs.PathDescribeRes{
			Stream: pa.stream,
		}
		return
	}

	// 处理按需静态源
	if pa.conf.HasOnDemandStaticSource() {
		if pa.onDemandStaticSourceState == pathOnDemandStateInitial {
			// 首次请求，启动按需静态源
			pa.onDemandStaticSourceStart(req.AccessRequest.Query)
		}
		// 将请求加入等待队列
		pa.describeRequestsOnHold = append(pa.describeRequestsOnHold, req)
		return
	}

	// 处理按需发布者
	if pa.conf.HasOnDemandPublisher() {
		if pa.onDemandPublisherState == pathOnDemandStateInitial {
			// 首次请求，启动按需发布者
			pa.onDemandPublisherStart(req.AccessRequest.Query)
		}
		// 将请求加入等待队列
		pa.describeRequestsOnHold = append(pa.describeRequestsOnHold, req)
		return
	}

	// 处理回退配置
	if pa.conf.Fallback != "" {
		req.Res <- defs.PathDescribeRes{Redirect: pa.conf.Fallback}
		return
	}

	// 没有可用的流
	req.Res <- defs.PathDescribeRes{Err: defs.PathNoStreamAvailableError{PathName: pa.name}}
}

// doRemovePublisher 处理移除发布者请求
// 当发布者断开连接时调用
func (pa *path) doRemovePublisher(req defs.PathRemovePublisherReq) {
	// 只有当请求的发布者确实是当前源时才执行移除
	if pa.source == req.Author {
		pa.executeRemovePublisher()
	}
	close(req.Res)
}

// doAddPublisher 处理添加发布者请求
// 当新的发布者请求连接到路径时调用
func (pa *path) doAddPublisher(req defs.PathAddPublisherReq) {
	// 检查路径是否允许发布者连接
	if pa.conf.Source != "publisher" {
		req.Res <- defs.PathAddPublisherRes{
			Err: fmt.Errorf("can't publish to path '%s' since 'source' is not 'publisher'", pa.name),
		}
		return
	}

	// 如果已有发布者，检查是否允许覆盖
	if pa.source != nil {
		if !pa.conf.OverridePublisher {
			req.Res <- defs.PathAddPublisherRes{Err: fmt.Errorf("someone is already publishing to path '%s'", pa.name)}
			return
		}

		// 关闭现有发布者
		pa.Log(logger.Info, "closing existing publisher")
		pa.source.(defs.Publisher).Close()
		pa.executeRemovePublisher()
	}

	// 设置新的发布者
	pa.source = req.Author
	pa.publisherQuery = req.AccessRequest.Query

	req.Res <- defs.PathAddPublisherRes{Path: pa}
}

// doStartPublisher 处理启动发布者请求
// 当发布者开始发布流时调用
func (pa *path) doStartPublisher(req defs.PathStartPublisherReq) {
	// 验证发布者身份
	if pa.source != req.Author {
		req.Res <- defs.PathStartPublisherRes{Err: fmt.Errorf("publisher is not assigned to this path anymore")}
		return
	}

	// 设置路径为就绪状态
	err := pa.setReady(req.Desc, req.GenerateRTPPackets)
	if err != nil {
		req.Res <- defs.PathStartPublisherRes{Err: err}
		return
	}

	// 记录发布者信息
	req.Author.Log(logger.Info, "is publishing to path '%s', %s",
		pa.name,
		defs.MediasInfo(req.Desc.Medias))

	// 如果是按需发布者，处理按需逻辑
	if pa.conf.HasOnDemandPublisher() && pa.onDemandPublisherState != pathOnDemandStateInitial {
		// 停止就绪定时器
		pa.onDemandPublisherReadyTimer.Stop()
		pa.onDemandPublisherReadyTimer = emptyTimer()
		// 安排关闭定时器
		pa.onDemandPublisherScheduleClose()
	}

	// 处理等待中的请求
	pa.consumeOnHoldRequests()

	req.Res <- defs.PathStartPublisherRes{Stream: pa.stream}
}

// doStopPublisher 处理停止发布者请求
// 当发布者停止发布流时调用
func (pa *path) doStopPublisher(req defs.PathStopPublisherReq) {
	// 只有当请求的发布者确实是当前源且流存在时才设置不可用
	if req.Author == pa.source && pa.stream != nil {
		pa.setNotReady()
	}
	close(req.Res)
}

// doAddReader 处理添加读取者请求
// 当新的读取者请求连接到路径时调用
func (pa *path) doAddReader(req defs.PathAddReaderReq) {
	// 如果流已就绪，直接添加读取者
	if pa.stream != nil {
		pa.addReaderPost(req)
		return
	}

	// 处理按需静态源
	if pa.conf.HasOnDemandStaticSource() {
		if pa.onDemandStaticSourceState == pathOnDemandStateInitial {
			// 首次请求，启动按需静态源
			pa.onDemandStaticSourceStart(req.AccessRequest.Query)
		}
		// 将请求加入等待队列
		pa.readerAddRequestsOnHold = append(pa.readerAddRequestsOnHold, req)
		return
	}

	// 处理按需发布者
	if pa.conf.HasOnDemandPublisher() {
		if pa.onDemandPublisherState == pathOnDemandStateInitial {
			// 首次请求，启动按需发布者
			pa.onDemandPublisherStart(req.AccessRequest.Query)
		}
		// 将请求加入等待队列
		pa.readerAddRequestsOnHold = append(pa.readerAddRequestsOnHold, req)
		return
	}

	// 没有可用的流
	req.Res <- defs.PathAddReaderRes{Err: defs.PathNoStreamAvailableError{PathName: pa.name}}
}

// doRemoveReader 处理移除读取者请求
// 当读取者断开连接时调用
func (pa *path) doRemoveReader(req defs.PathRemoveReaderReq) {
	// 从读取者集合中移除
	if _, ok := pa.readers[req.Author]; ok {
		pa.executeRemoveReader(req.Author)
	}
	close(req.Res)

	// 如果没有读取者了，考虑关闭按需源
	if len(pa.readers) == 0 {
		if pa.conf.HasOnDemandStaticSource() {
			if pa.onDemandStaticSourceState == pathOnDemandStateReady {
				// 安排关闭按需静态源
				pa.onDemandStaticSourceScheduleClose()
			}
		} else if pa.conf.HasOnDemandPublisher() {
			if pa.onDemandPublisherState == pathOnDemandStateReady {
				// 安排关闭按需发布者
				pa.onDemandPublisherScheduleClose()
			}
		}
	}
}

// doAPIPathsGet 处理API获取路径信息请求
// 通过HTTP API获取路径的详细状态信息
func (pa *path) doAPIPathsGet(req pathAPIPathsGetReq) {
	req.res <- pathAPIPathsGetRes{
		data: &defs.APIPath{
			Name:     pa.name,      // 路径名称
			ConfName: pa.conf.Name, // 配置名称

			// 源信息
			Source: func() *defs.APIPathSourceOrReader {
				if pa.source == nil {
					return nil
				}
				v := pa.source.APISourceDescribe()
				return &v
			}(),

			// 就绪状态
			Ready: pa.isReady(),
			ReadyTime: func() *time.Time {
				if !pa.isReady() {
					return nil
				}
				v := pa.readyTime
				return &v
			}(),

			// 轨道信息
			Tracks: func() []string {
				if !pa.isReady() {
					return []string{}
				}
				return defs.MediasToCodecs(pa.stream.Desc.Medias)
			}(),

			// 数据统计
			BytesReceived: func() uint64 {
				if !pa.isReady() {
					return 0
				}
				return pa.stream.BytesReceived()
			}(),
			BytesSent: func() uint64 {
				if !pa.isReady() {
					return 0
				}
				return pa.stream.BytesSent()
			}(),

			// 读取者列表
			Readers: func() []defs.APIPathSourceOrReader {
				ret := []defs.APIPathSourceOrReader{}
				for r := range pa.readers {
					ret = append(ret, r.APIReaderDescribe())
				}
				return ret
			}(),
		},
	}
}

// SafeConf 安全地获取路径配置
// 使用读锁保护，确保并发安全
func (pa *path) SafeConf() *conf.Path {
	pa.confMutex.RLock()
	defer pa.confMutex.RUnlock()
	return pa.conf
}

// ExternalCmdEnv 获取外部命令的环境变量
// 为外部脚本提供路径相关的环境变量
func (pa *path) ExternalCmdEnv() externalcmd.Environment {
	// 解析RTSP地址获取端口
	_, port, _ := net.SplitHostPort(pa.rtspAddress)
	env := externalcmd.Environment{
		"MTX_PATH":  pa.name, // 路径名称
		"RTSP_PATH": pa.name, // 路径名称（已废弃，保持兼容性）
		"RTSP_PORT": port,    // RTSP端口
	}

	// 如果路径名称包含正则表达式匹配组，添加到环境变量
	if len(pa.matches) > 1 {
		for i, ma := range pa.matches[1:] {
			env["G"+strconv.FormatInt(int64(i+1), 10)] = ma
		}
	}

	return env
}

// shouldClose 判断路径是否应该关闭
// 当路径是正则表达式匹配的且没有任何活动时，可以关闭
func (pa *path) shouldClose() bool {
	return pa.conf.Regexp != nil && // 是正则表达式匹配的路径
		pa.source == nil && // 没有源
		len(pa.readers) == 0 && // 没有读取者
		len(pa.describeRequestsOnHold) == 0 && // 没有等待的描述请求
		len(pa.readerAddRequestsOnHold) == 0 // 没有等待的读取者添加请求
}

// onDemandStaticSourceStart 启动按需静态源
// 当有客户端请求时，按需启动静态源
func (pa *path) onDemandStaticSourceStart(query string) {
	// 启动静态源处理器
	pa.source.(*staticsources.Handler).Start(true, query)

	// 设置就绪超时定时器
	pa.onDemandStaticSourceReadyTimer.Stop()
	pa.onDemandStaticSourceReadyTimer = time.NewTimer(time.Duration(pa.conf.SourceOnDemandStartTimeout))

	// 更新状态为等待就绪
	pa.onDemandStaticSourceState = pathOnDemandStateWaitingReady
}

// onDemandStaticSourceScheduleClose 安排关闭按需静态源
// 当没有读取者时，安排延迟关闭静态源
func (pa *path) onDemandStaticSourceScheduleClose() {
	// 设置关闭延迟定时器
	pa.onDemandStaticSourceCloseTimer.Stop()
	pa.onDemandStaticSourceCloseTimer = time.NewTimer(time.Duration(pa.conf.SourceOnDemandCloseAfter))

	// 更新状态为关闭中
	pa.onDemandStaticSourceState = pathOnDemandStateClosing
}

// onDemandStaticSourceStop 停止按需静态源
// 停止静态源并清理相关资源
func (pa *path) onDemandStaticSourceStop(reason string) {
	// 如果正在关闭中，停止关闭定时器
	if pa.onDemandStaticSourceState == pathOnDemandStateClosing {
		pa.onDemandStaticSourceCloseTimer.Stop()
		pa.onDemandStaticSourceCloseTimer = emptyTimer()
	}

	// 重置状态为初始状态
	pa.onDemandStaticSourceState = pathOnDemandStateInitial

	// 停止静态源处理器
	pa.source.(*staticsources.Handler).Stop(reason)
}

// onDemandPublisherStart 启动按需发布者
// 当有客户端请求时，按需启动发布者
func (pa *path) onDemandPublisherStart(query string) {
	// 执行按需启动钩子
	pa.onUnDemandHook = hooks.OnDemand(hooks.OnDemandParams{
		Logger:          pa,
		ExternalCmdPool: pa.externalCmdPool,
		Conf:            pa.conf,
		ExternalCmdEnv:  pa.ExternalCmdEnv(),
		Query:           query,
	})

	// 设置就绪超时定时器
	pa.onDemandPublisherReadyTimer.Stop()
	pa.onDemandPublisherReadyTimer = time.NewTimer(time.Duration(pa.conf.RunOnDemandStartTimeout))

	// 更新状态为等待就绪
	pa.onDemandPublisherState = pathOnDemandStateWaitingReady
}

// onDemandPublisherScheduleClose 安排关闭按需发布者
// 当没有读取者时，安排延迟关闭发布者
func (pa *path) onDemandPublisherScheduleClose() {
	// 设置关闭延迟定时器
	pa.onDemandPublisherCloseTimer.Stop()
	pa.onDemandPublisherCloseTimer = time.NewTimer(time.Duration(pa.conf.RunOnDemandCloseAfter))

	// 更新状态为关闭中
	pa.onDemandPublisherState = pathOnDemandStateClosing
}

// onDemandPublisherStop 停止按需发布者
// 停止发布者并清理相关资源
func (pa *path) onDemandPublisherStop(reason string) {
	// 如果正在关闭中，停止关闭定时器
	if pa.onDemandPublisherState == pathOnDemandStateClosing {
		pa.onDemandPublisherCloseTimer.Stop()
		pa.onDemandPublisherCloseTimer = emptyTimer()
	}

	// 执行按需停止钩子
	pa.onUnDemandHook(reason)
	pa.onUnDemandHook = nil

	// 重置状态为初始状态
	pa.onDemandPublisherState = pathOnDemandStateInitial
}

// setReady 设置路径为就绪状态
// 创建流对象、启动录制、执行就绪钩子
func (pa *path) setReady(desc *description.Session, allocateEncoder bool) error {
	// 创建流对象
	pa.stream = &stream.Stream{
		WriteQueueSize:     pa.writeQueueSize,
		RTPMaxPayloadSize:  pa.rtpMaxPayloadSize,
		Desc:               desc,
		GenerateRTPPackets: allocateEncoder,
		Parent:             pa.source,
	}

	// 初始化流
	err := pa.stream.Initialize()
	if err != nil {
		return err
	}

	// 如果启用了录制，启动录制器
	if pa.conf.Record {
		pa.startRecording()
	}

	// 记录就绪时间
	pa.readyTime = time.Now()

	// 执行就绪钩子
	pa.onNotReadyHook = hooks.OnReady(hooks.OnReadyParams{
		Logger:          pa,
		ExternalCmdPool: pa.externalCmdPool,
		Conf:            pa.conf,
		ExternalCmdEnv:  pa.ExternalCmdEnv(),
		Desc:            pa.source.APISourceDescribe(),
		Query:           pa.publisherQuery,
	})

	// 通知父级路径已就绪
	pa.parent.pathReady(pa)

	return nil
}

// consumeOnHoldRequests 处理等待中的请求
// 当路径就绪时，处理之前等待的描述请求和读取者添加请求
func (pa *path) consumeOnHoldRequests() {
	// 处理等待中的描述请求
	for _, req := range pa.describeRequestsOnHold {
		req.Res <- defs.PathDescribeRes{
			Stream: pa.stream,
		}
	}
	pa.describeRequestsOnHold = nil

	// 处理等待中的读取者添加请求
	for _, req := range pa.readerAddRequestsOnHold {
		pa.addReaderPost(req)
	}
	pa.readerAddRequestsOnHold = nil
}

// setNotReady 设置路径为不可用状态
// 清理所有读取者、停止录制、关闭流
func (pa *path) setNotReady() {
	// 通知父级路径不可用
	pa.parent.pathNotReady(pa)

	// 关闭所有读取者
	for r := range pa.readers {
		pa.executeRemoveReader(r)
		r.Close()
	}

	// 执行不可用钩子
	pa.onNotReadyHook()

	// 关闭录制器
	if pa.recorder != nil {
		pa.recorder.Close()
		pa.recorder = nil
	}

	// 关闭流
	if pa.stream != nil {
		pa.stream.Close()
		pa.stream = nil
	}
}

// startRecording 启动录制功能
// 创建录制器并配置相关回调
func (pa *path) startRecording() {
	pa.recorder = &recorder.Recorder{
		PathFormat:      pa.conf.RecordPath,                           // 录制文件路径格式
		Format:          pa.conf.RecordFormat,                         // 录制格式
		PartDuration:    time.Duration(pa.conf.RecordPartDuration),    // 分片时长
		SegmentDuration: time.Duration(pa.conf.RecordSegmentDuration), // 片段时长
		PathName:        pa.name,                                      // 路径名称
		Stream:          pa.stream,                                    // 流对象

		// 片段创建回调
		OnSegmentCreate: func(segmentPath string) {
			if pa.conf.RunOnRecordSegmentCreate != "" {
				env := pa.ExternalCmdEnv()
				env["MTX_SEGMENT_PATH"] = segmentPath

				pa.Log(logger.Info, "runOnRecordSegmentCreate command launched")
				externalcmd.NewCmd(
					pa.externalCmdPool,
					pa.conf.RunOnRecordSegmentCreate,
					false,
					env,
					nil)
			}
		},

		// 片段完成回调
		OnSegmentComplete: func(segmentPath string, segmentDuration time.Duration) {
			if pa.conf.RunOnRecordSegmentComplete != "" {
				env := pa.ExternalCmdEnv()
				env["MTX_SEGMENT_PATH"] = segmentPath
				env["MTX_SEGMENT_DURATION"] = strconv.FormatFloat(segmentDuration.Seconds(), 'f', -1, 64)

				pa.Log(logger.Info, "runOnRecordSegmentComplete command launched")
				externalcmd.NewCmd(
					pa.externalCmdPool,
					pa.conf.RunOnRecordSegmentComplete,
					false,
					env,
					nil)
			}
		},
		Parent: pa,
	}
	pa.recorder.Initialize()
}

// executeRemoveReader 执行移除读取者操作
// 从读取者集合中移除指定的读取者
func (pa *path) executeRemoveReader(r defs.Reader) {
	delete(pa.readers, r)
}

// executeRemovePublisher 执行移除发布者操作
// 清理发布者相关资源并重置源
func (pa *path) executeRemovePublisher() {
	// 如果流存在，设置为不可用状态
	if pa.stream != nil {
		pa.setNotReady()
	}

	// 清空源
	pa.source = nil
}

// addReaderPost 添加读取者（流已就绪的情况）
// 当流已经就绪时，直接添加读取者
func (pa *path) addReaderPost(req defs.PathAddReaderReq) {
	// 检查读取者是否已存在
	if _, ok := pa.readers[req.Author]; ok {
		req.Res <- defs.PathAddReaderRes{
			Path:   pa,
			Stream: pa.stream,
		}
		return
	}

	// 检查读取者数量限制
	if pa.conf.MaxReaders != 0 && len(pa.readers) >= pa.conf.MaxReaders {
		req.Res <- defs.PathAddReaderRes{Err: fmt.Errorf("maximum reader count reached")}
		return
	}

	// 添加读取者到集合
	pa.readers[req.Author] = struct{}{}

	// 处理按需逻辑
	if pa.conf.HasOnDemandStaticSource() {
		if pa.onDemandStaticSourceState == pathOnDemandStateClosing {
			// 如果有读取者连接，取消关闭计划
			pa.onDemandStaticSourceState = pathOnDemandStateReady
			pa.onDemandStaticSourceCloseTimer.Stop()
			pa.onDemandStaticSourceCloseTimer = emptyTimer()
		}
	} else if pa.conf.HasOnDemandPublisher() {
		if pa.onDemandPublisherState == pathOnDemandStateClosing {
			// 如果有读取者连接，取消关闭计划
			pa.onDemandPublisherState = pathOnDemandStateReady
			pa.onDemandPublisherCloseTimer.Stop()
			pa.onDemandPublisherCloseTimer = emptyTimer()
		}
	}

	// 返回成功响应
	req.Res <- defs.PathAddReaderRes{
		Path:   pa,
		Stream: pa.stream,
	}
}

// reloadConf 重新加载配置（由pathManager调用）
// 通过通道发送配置重载请求到主循环
func (pa *path) reloadConf(newConf *conf.Path) {
	select {
	case pa.chReloadConf <- newConf:
	case <-pa.ctx.Done():
	}
}

// StaticSourceHandlerSetReady 静态源处理器设置就绪（由staticsources.Handler调用）
// 当静态源成功启动并准备就绪时调用
func (pa *path) StaticSourceHandlerSetReady(
	ctx context.Context, req defs.PathSourceStaticSetReadyReq,
) {
	select {
	case pa.chStaticSourceSetReady <- req:

	case <-pa.ctx.Done():
		req.Res <- defs.PathSourceStaticSetReadyRes{Err: fmt.Errorf("terminated")}

	// 避免以下情况：
	// - 在源终止后发送无效请求
	// - 由于在stop()内部等待done导致的死锁
	case <-ctx.Done():
		req.Res <- defs.PathSourceStaticSetReadyRes{Err: fmt.Errorf("terminated")}
	}
}

// StaticSourceHandlerSetNotReady 静态源处理器设置不可用（由staticsources.Handler调用）
// 当静态源出现错误或停止时调用
func (pa *path) StaticSourceHandlerSetNotReady(
	ctx context.Context, req defs.PathSourceStaticSetNotReadyReq,
) {
	select {
	case pa.chStaticSourceSetNotReady <- req:

	case <-pa.ctx.Done():
		close(req.Res)

	// 避免以下情况：
	// - 在源终止后发送无效请求
	// - 由于在stop()内部等待done导致的死锁
	case <-ctx.Done():
		close(req.Res)
	}
}

// describe 描述请求（由读取者或发布者通过pathManager调用）
// 获取路径的流描述信息
func (pa *path) describe(req defs.PathDescribeReq) defs.PathDescribeRes {
	select {
	case pa.chDescribe <- req:
		return <-req.Res
	case <-pa.ctx.Done():
		return defs.PathDescribeRes{Err: fmt.Errorf("terminated")}
	}
}

// addPublisher 添加发布者（由发布者通过pathManager调用）
// 请求连接到路径作为发布者
func (pa *path) addPublisher(req defs.PathAddPublisherReq) (defs.Path, error) {
	select {
	case pa.chAddPublisher <- req:
		res := <-req.Res
		return res.Path, res.Err
	case <-pa.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// RemovePublisher 移除发布者（由发布者调用）
// 断开发布者连接
func (pa *path) RemovePublisher(req defs.PathRemovePublisherReq) {
	req.Res = make(chan struct{})
	select {
	case pa.chRemovePublisher <- req:
		<-req.Res
	case <-pa.ctx.Done():
	}
}

// StartPublisher 启动发布者（由发布者调用）
// 开始发布流
func (pa *path) StartPublisher(req defs.PathStartPublisherReq) (*stream.Stream, error) {
	req.Res = make(chan defs.PathStartPublisherRes)
	select {
	case pa.chStartPublisher <- req:
		res := <-req.Res
		return res.Stream, res.Err
	case <-pa.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// StopPublisher 停止发布者（由发布者调用）
// 停止发布流
func (pa *path) StopPublisher(req defs.PathStopPublisherReq) {
	req.Res = make(chan struct{})
	select {
	case pa.chStopPublisher <- req:
		<-req.Res
	case <-pa.ctx.Done():
	}
}

// addReader 添加读取者（由读取者通过pathManager调用）
// 请求连接到路径作为读取者
func (pa *path) addReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error) {
	select {
	case pa.chAddReader <- req:
		res := <-req.Res
		return res.Path, res.Stream, res.Err
	case <-pa.ctx.Done():
		return nil, nil, fmt.Errorf("terminated")
	}
}

// RemoveReader 移除读取者（由读取者调用）
// 断开读取者连接
func (pa *path) RemoveReader(req defs.PathRemoveReaderReq) {
	req.Res = make(chan struct{})
	select {
	case pa.chRemoveReader <- req:
		<-req.Res
	case <-pa.ctx.Done():
	}
}

// APIPathsGet API获取路径信息（由API调用）
// 通过HTTP API获取路径的详细状态信息
func (pa *path) APIPathsGet(req pathAPIPathsGetReq) (*defs.APIPath, error) {
	req.res = make(chan pathAPIPathsGetRes)
	select {
	case pa.chAPIPathsGet <- req:
		res := <-req.res
		return res.data, res.err

	case <-pa.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}
