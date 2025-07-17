#!/bin/bash

# 高级流处理脚本
# 演示多种处理方式：转码、水印、多路输出等

INPUT_STREAM="rtsp://localhost:8554/raw_stream"
OUTPUT_BASE="rtsp://localhost:8554"

echo "=== 高级流处理器启动 ==="
echo "输入流: $INPUT_STREAM"
echo ""

# 1. 基础转码处理
process_basic() {
    echo "启动基础转码处理..."
    ffmpeg -i "$INPUT_STREAM" \
           -c:v libx264 -preset fast -crf 23 \
           -c:a aac -b:a 128k \
           -f rtsp "$OUTPUT_BASE/processed_stream" &
    
    echo "基础转码已启动"
}

# 2. 高清转码处理
process_hd() {
    echo "启动高清转码处理..."
    ffmpeg -i "$INPUT_STREAM" \
           -c:v libx264 -preset medium -crf 18 \
           -c:a aac -b:a 192k \
           -s 1920x1080 \
           -f rtsp "$OUTPUT_BASE/hd_stream" &
    
    echo "高清转码已启动"
}

# 3. 低延迟处理
process_lowlatency() {
    echo "启动低延迟处理..."
    ffmpeg -i "$INPUT_STREAM" \
           -c:v libx264 -preset ultrafast -tune zerolatency \
           -c:a aac -b:a 64k \
           -g 30 -keyint_min 30 \
           -f rtsp "$OUTPUT_BASE/lowlatency_stream" &
    
    echo "低延迟处理已启动"
}

# 4. 添加水印处理
process_watermark() {
    echo "启动水印处理..."
    ffmpeg -i "$INPUT_STREAM" \
           -i watermark.png \
           -filter_complex "overlay=10:10" \
           -c:v libx264 -preset fast -crf 23 \
           -c:a aac -b:a 128k \
           -f rtsp "$OUTPUT_BASE/watermark_stream" &
    
    echo "水印处理已启动"
}

# 5. 多路输出处理
process_multiple() {
    echo "启动多路输出处理..."
    ffmpeg -i "$INPUT_STREAM" \
           -c:v libx264 -preset fast -crf 23 \
           -c:a aac -b:a 128k \
           -f rtsp "$OUTPUT_BASE/multi_1" \
           -c:v libx264 -preset fast -crf 28 \
           -c:a aac -b:a 64k \
           -s 640x480 \
           -f rtsp "$OUTPUT_BASE/multi_2" &
    
    echo "多路输出已启动"
}

# 6. 录制处理
process_record() {
    echo "启动录制处理..."
    ffmpeg -i "$INPUT_STREAM" \
           -c:v libx264 -preset fast -crf 23 \
           -c:a aac -b:a 128k \
           -f segment -segment_time 60 \
           -reset_timestamps 1 \
           "./recordings/segments/segment_%03d.mp4" &
    
    echo "录制处理已启动"
}

# 7. 实时分析处理
process_analytics() {
    echo "启动实时分析处理..."
    ffmpeg -i "$INPUT_STREAM" \
           -c:v libx264 -preset ultrafast \
           -c:a aac -b:a 64k \
           -f rtsp "$OUTPUT_BASE/analytics_stream" \
           -f null - 2>&1 | grep -E "(frame|fps|bitrate)" &
    
    echo "实时分析已启动"
}

# 主处理函数
main() {
    echo "选择处理模式："
    echo "1. 基础转码"
    echo "2. 高清转码"
    echo "3. 低延迟"
    echo "4. 添加水印"
    echo "5. 多路输出"
    echo "6. 录制处理"
    echo "7. 实时分析"
    echo "8. 全部启动"
    echo ""
    
    read -p "请输入选择 (1-8): " choice
    
    case $choice in
        1) process_basic ;;
        2) process_hd ;;
        3) process_lowlatency ;;
        4) process_watermark ;;
        5) process_multiple ;;
        6) process_record ;;
        7) process_analytics ;;
        8) 
            process_basic
            sleep 1
            process_hd
            sleep 1
            process_lowlatency
            sleep 1
            process_multiple
            sleep 1
            process_record
            ;;
        *) echo "无效选择" ;;
    esac
    
    echo ""
    echo "处理已启动，按 Ctrl+C 停止"
    
    # 等待用户中断
    wait
}

# 清理函数
cleanup() {
    echo ""
    echo "正在停止所有处理进程..."
    pkill -f "ffmpeg.*localhost:8554"
    echo "清理完成"
    exit 0
}

# 设置信号处理
trap cleanup SIGINT SIGTERM

# 创建目录
mkdir -p recordings/segments

# 运行主函数
main 