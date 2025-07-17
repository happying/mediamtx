# MediaMTX 项目学习路径指南

## 🎯 学习目标
通过系统性的代码阅读，理解MediaMTX作为流媒体服务器的完整工作流程，从启动到处理客户端请求的全过程。

## 📍 学习起点：main.go

### 第一步：程序入口分析
**文件位置**: `main.go`

```go
func main() {
    s, ok := core.New(os.Args[1:])  // 创建Core实例
    if !ok {
        os.Exit(1)
    }
    s.Wait()  // 等待程序结束
}
```

**学习要点**:
- 程序如何解析命令行参数
- Core结构体的创建过程
- 程序的生命周期管理

**设置断点**: `main.go:10` (core.New调用)

---

## 🔧 第二步：Core初始化流程

### 2.1 Core结构体分析
**文件位置**: `internal/core/core.go:77-105`

```go
type Core struct {
    ctx             context.Context
    ctxCancel       func()
    confPath        string
    conf            *conf.Conf
    logger          *logger.Logger
    externalCmdPool *externalcmd.Pool
    authManager     *auth.Manager
    metrics         *metrics.Metrics
    pprof           *pprof.PPROF
    recordCleaner   *recordcleaner.Cleaner
    playbackServer  *playback.Server
    pathManager     *pathManager
    rtspServer      *rtsp.Server
    rtspsServer     *rtsp.Server
    rtmpServer      *rtmp.Server
    rtmpsServer     *rtmp.Server
    hlsServer       *hls.Server
    webRTCServer    *webrtc.Server
    srtServer       *srt.Server
    api             *api.API
    confWatcher     *confwatcher.ConfWatcher
    // ...
}
```

**学习要点**:
- 理解每个组件的职责
- 组件间的依赖关系
- 整体架构设计

**设置断点**: `internal/core/core.go:108` (New函数开始)

### 2.2 配置加载流程
**文件位置**: `internal/core/core.go:140-150`

```go
p.conf, p.confPath, err = conf.Load(cli.Confpath, defaultConfPaths, tempLogger)
```

**学习要点**:
- 配置文件如何被加载和解析
- 默认配置的处理
- 配置验证机制

**设置断点**: `internal/conf/conf.go` (Load函数)

### 2.3 资源创建流程
**文件位置**: `internal/core/core.go:234-362`

```go
err = p.createResources(true)
```

**学习要点**:
- 各个服务器组件的初始化顺序
- 错误处理机制
- 资源管理策略

**设置断点**: `internal/core/core.go:234` (createResources函数)

---

## 🚀 第三步：服务器启动流程

### 3.1 主事件循环
**文件位置**: `internal/core/core.go:170-220`

```go
func (p *Core) run() {
    defer close(p.done)
    
    // 配置变更监听
    confChanged := func() chan struct{} {
        if p.confWatcher != nil {
            return p.confWatcher.Watch()
        }
        return make(chan struct{})
    }()
    
    // 信号处理
    interrupt := make(chan os.Signal, 1)
    signal.Notify(interrupt, os.Interrupt)
    
    // 主事件循环
    for {
        select {
        case <-confChanged:
            // 配置热重载
        case newConf := <-p.chAPIConfigSet:
            // API配置更新
        case <-interrupt:
            // 优雅关闭
        case <-p.ctx.Done():
            // 上下文取消
        }
    }
}
```

**学习要点**:
- Go语言的并发模型应用
- 事件驱动架构
- 优雅关闭机制

**设置断点**: `internal/core/core.go:170` (run函数开始)

---

## 📡 第四步：协议服务器实现

### 4.1 RTSP服务器 (重点推荐)
**文件位置**: `internal/servers/rtsp/`

**学习路径**:
1. `server.go` - 服务器主逻辑
2. `conn.go` - 连接处理
3. `session.go` - 会话管理

**核心概念**:
- RTSP协议状态机
- RTP/RTCP媒体传输
- 会话生命周期管理

**设置断点**: `internal/servers/rtsp/server.go` (Server结构体)

### 4.2 WebRTC服务器
**文件位置**: `internal/servers/webrtc/`

**学习路径**:
1. `server.go` - WebRTC服务器
2. `publisher.go` - 发布者处理
3. `reader.go` - 读取者处理

**核心概念**:
- ICE连接建立
- SDP协商过程
- DTLS加密传输

**设置断点**: `internal/servers/webrtc/server.go`

### 4.3 其他协议服务器
- RTMP: `internal/servers/rtmp/`
- HLS: `internal/servers/hls/`
- SRT: `internal/servers/srt/`

---

## 🛣️ 第五步：路径管理系统

### 5.1 路径管理器
**文件位置**: `internal/core/path_manager.go`

**学习要点**:
- 路径的创建和销毁
- 发布者和读取者的管理
- 流媒体数据的路由

**设置断点**: `internal/core/path_manager.go:64` (pathManager结构体)

### 5.2 路径数据流
**文件位置**: `internal/core/path.go`

**学习要点**:
- 媒体数据的接收和转发
- 缓冲区管理
- 多协议转换

---

## 🔐 第六步：认证和权限系统

### 6.1 认证管理器
**文件位置**: `internal/auth/`

**学习要点**:
- 多种认证方式实现
- 权限验证机制
- JWT令牌处理

**设置断点**: `internal/auth/manager.go`

---

## 📊 第七步：API和监控系统

### 7.1 RESTful API
**文件位置**: `internal/api/`

**学习要点**:
- HTTP路由设计
- JSON序列化
- 配置热更新

**设置断点**: `internal/api/api.go:146` (路由设置)

### 7.2 指标监控
**文件位置**: `internal/metrics/`

**学习要点**:
- Prometheus格式指标
- 性能数据收集
- 监控端点实现

---

## 🎬 第八步：录制和回放系统

### 8.1 录制功能
**文件位置**: `internal/recorder/`

**学习要点**:
- 媒体文件格式处理
- 分段录制策略
- 存储管理

### 8.2 回放功能
**文件位置**: `internal/playback/`

**学习要点**:
- HTTP流媒体服务
- 时间戳处理
- 文件索引管理

---

## 🔧 实践练习

### 练习1：跟踪RTSP连接流程
1. 启动调试模式
2. 使用FFmpeg发布RTSP流
3. 跟踪从连接到数据传输的完整流程

### 练习2：分析WebRTC连接建立
1. 在浏览器中访问WebRTC页面
2. 跟踪ICE连接建立过程
3. 观察SDP协商过程

### 练习3：配置热重载测试
1. 修改配置文件
2. 观察配置重载过程
3. 验证新配置是否生效

---

## 📚 学习建议

### 1. 循序渐进
- 从main.go开始，逐步深入各个模块
- 每个模块都要有实践验证
- 保持代码阅读和实际操作的平衡

### 2. 重点关注
- **并发处理**: 理解Go语言的goroutine和channel使用
- **协议实现**: 重点学习RTSP和WebRTC的实现
- **错误处理**: 观察项目中的错误处理策略
- **性能优化**: 理解缓冲区管理和内存使用

### 3. 调试技巧
- 使用VSCode的调试功能设置断点
- 观察变量值和调用栈
- 使用日志输出跟踪程序流程
- 利用pprof进行性能分析

### 4. 扩展学习
- 阅读相关协议规范（RFC文档）
- 学习Go语言并发编程最佳实践
- 了解流媒体技术基础知识

---

## 🎯 学习里程碑

### 第一阶段 (1-2周)
- [ ] 理解程序启动流程
- [ ] 掌握配置加载机制
- [ ] 熟悉Core架构设计

### 第二阶段 (2-3周)
- [ ] 深入RTSP协议实现
- [ ] 理解路径管理系统
- [ ] 掌握认证机制

### 第三阶段 (2-3周)
- [ ] 学习WebRTC实现
- [ ] 理解API系统设计
- [ ] 掌握录制回放功能

### 第四阶段 (1-2周)
- [ ] 性能优化技巧
- [ ] 错误处理策略
- [ ] 项目扩展方法

---

## 🚀 开始学习

现在您可以：

1. **打开VSCode**，加载MediaMTX项目
2. **设置断点**在 `main.go:10`
3. **启动调试**，选择"Debug MediaMTX with Custom Config"
4. **开始跟踪**程序执行流程

祝您学习愉快！🎉 