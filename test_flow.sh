#!/bin/bash

echo "=== MediaMTX 收流-处理-推流测试流程 ==="
echo ""

# 创建录制目录
mkdir -p recordings/{raw,processed,test}

# 检查MediaMTX是否运行
check_mediamtx() {
    echo "1. 检查MediaMTX服务器状态..."
    if curl -s http://localhost:9997/v3/paths/list > /dev/null 2>&1; then
        echo "   ✅ MediaMTX服务器运行正常"
        return 0
    else
        echo "   ❌ MediaMTX服务器未运行，请先启动："
        echo "      ./mediamtx test_flow.yml"
        return 1
    fi
}

# 生成测试视频流
generate_test_stream() {
    echo ""
    echo "2. 生成测试视频流..."
    echo "   使用ffmpeg生成测试视频并推送到raw_stream路径"
    
    # 生成测试视频流（10秒循环）
    ffmpeg -re -f lavfi -i testsrc=duration=10:size=640x480:rate=30 \
           -f lavfi -i sine=frequency=1000:duration=10 \
           -c:v libx264 -preset ultrafast -tune zerolatency \
           -c:a aac -b:a 128k \
           -f rtsp rtsp://localhost:8554/raw_stream &
    
    FFMPEG_PID=$!
    echo "   ✅ 测试流生成器已启动 (PID: $FFMPEG_PID)"
    echo "   流地址: rtsp://localhost:8554/raw_stream"
}

# 监控流状态
monitor_streams() {
    echo ""
    echo "3. 监控流状态..."
    echo "   按 Ctrl+C 停止监控"
    
    while true; do
        echo ""
        echo "=== 当前流状态 ==="
        
        # 检查原始流
        if curl -s http://localhost:9997/v3/paths/raw_stream/get | grep -q '"ready":true'; then
            echo "   ✅ raw_stream: 运行中"
        else
            echo "   ❌ raw_stream: 未运行"
        fi
        
        # 检查处理后流
        if curl -s http://localhost:9997/v3/paths/processed_stream/get | grep -q '"ready":true'; then
            echo "   ✅ processed_stream: 运行中"
        else
            echo "   ❌ processed_stream: 未运行"
        fi
        
        # 检查测试流
        if curl -s http://localhost:9997/v3/paths/test/get | grep -q '"ready":true'; then
            echo "   ✅ test: 运行中"
        else
            echo "   ❌ test: 未运行"
        fi
        
        echo ""
        echo "=== 播放地址 ==="
        echo "   原始流: rtsp://localhost:8554/raw_stream"
        echo "   处理后流: rtsp://localhost:8554/processed_stream"
        echo "   测试流: rtsp://localhost:8554/test"
        echo "   HLS播放: http://localhost:8888/raw_stream/index.m3u8"
        echo "   HLS播放: http://localhost:8888/processed_stream/index.m3u8"
        
        sleep 5
    done
}

# 清理函数
cleanup() {
    echo ""
    echo "正在清理..."
    
    # 停止ffmpeg进程
    if [ ! -z "$FFMPEG_PID" ]; then
        kill $FFMPEG_PID 2>/dev/null
        echo "已停止测试流生成器"
    fi
    
    # 停止所有ffmpeg处理进程
    pkill -f "ffmpeg.*raw_stream" 2>/dev/null
    echo "已停止处理脚本"
    
    echo "清理完成"
    exit 0
}

# 设置信号处理
trap cleanup SIGINT SIGTERM

# 主流程
main() {
    # 检查MediaMTX状态
    if ! check_mediamtx; then
        exit 1
    fi
    
    # 生成测试流
    generate_test_stream
    
    # 等待一下让流启动
    sleep 3
    
    # 监控流状态
    monitor_streams
}

# 运行主流程
main 