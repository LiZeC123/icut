package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 内嵌 HTML ----------

//go:embed index.html
var htmlPage string

// ---------- 全局状态 ----------

var (
	rootDir     string
	port        int
	minSize     int64
	videos      []Video
	videosMu    sync.RWMutex
	whitelist   map[string]string
	whitelistMu sync.RWMutex

	storyboards     map[string]*Storyboard
	storyboardsMu   sync.RWMutex
	storyboardQueue chan string

	screenshotMu sync.Mutex
	clipMu       sync.Mutex

	storyboardWorkers = 2
)

type Video struct {
	RelPath  string  `json:"relPath"`
	AbsPath  string  `json:"absPath"`
	Size     int64   `json:"size"`
	BaseName string  `json:"baseName"`
	Ext      string  `json:"ext"`
	Duration float64 `json:"duration,omitempty"`
}

type Storyboard struct {
	Data []byte
	Err  error
}

// ---------- 日志 ----------

func truncateStr(s string, max int) string {
	if len(s) > max {
		return s[:max] + "...(已截断)"
	}
	return s
}

func logMsg(msg string) {
	msg = truncateStr(msg, 2048)
	fmt.Println(time.Now().Format("15:04:05"), msg)
}

// ---------- ffmpeg 执行 ----------

func runFFmpeg(timeout time.Duration, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errBuf.String(), fmt.Errorf("ffmpeg 超时")
	}
	return outBuf.Bytes(), errBuf.String(), err
}

func getDuration(absPath string) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", absPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}

func isVideoExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mp4", ".mov", ".mkv", ".avi", ".flv", ".wmv", ".webm", ".m4v", ".mpg", ".mpeg", ".ts":
		return true
	}
	return false
}

// ---------- 扫描 ----------

func scanVideos() {
	whitelist = make(map[string]string)
	filepath.Walk(rootDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !isVideoExt(filepath.Ext(p)) {
			return nil
		}
		if info.Size() < minSize {
			return nil
		}
		rel, err := filepath.Rel(rootDir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		base := filepath.Base(rel)
		ext := filepath.Ext(base)
		baseNoExt := strings.TrimSuffix(base, ext)

		videos = append(videos, Video{
			RelPath:  rel,
			AbsPath:  p,
			Size:     info.Size(),
			BaseName: baseNoExt,
			Ext:      ext,
		})
		whitelist[rel] = p
		return nil
	})

	sort.Slice(videos, func(i, j int) bool {
		return videos[i].RelPath < videos[j].RelPath
	})

	nameCount := map[string]int{}
	for _, v := range videos {
		nameCount[v.BaseName]++
	}
	for name, cnt := range nameCount {
		if cnt > 1 {
			logMsg(fmt.Sprintf("重复文件名告警: %s (%d 次)", name, cnt))
		}
	}
}

// ---------- 故事板缓存加载 ----------

// EnsureStoryDir 检查 rootDir 下是否存在 story 目录，不存在则创建，
// 最后返回指向 story 目录的完整路径。
func ensureSubDir(rootDir, subName string) (string, error) {
	// 如果 rootDir 为空，退化为当前目录
	if rootDir == "" {
		rootDir = "."
	}

	// 拼接 story 目录路径
	storyDir := filepath.Join(rootDir, subName)

	// 检查路径是否已存在
	info, err := os.Stat(storyDir)
	switch {
	case err == nil:
		// 路径存在，但必须是一个目录
		if !info.IsDir() {
			return "", fmt.Errorf("路径已存在但不是目录: %s", storyDir)
		}
		// 已存在目录，直接返回
		return storyDir, nil

	case errors.Is(err, os.ErrNotExist):
		// 目录不存在，创建（MkdirAll 会自动创建缺失的父级目录）
		if err := os.MkdirAll(storyDir, 0o755); err != nil {
			return "", fmt.Errorf("创建 sub 目录失败: %w", err)
		}
		return storyDir, nil

	default:
		// 其他错误（权限不足、路径非法等）
		return "", fmt.Errorf("检查 sub 目录失败: %w", err)
	}
}

// 修改点：加 mtime 判定（1 秒容差），缓存过期则重新入队
func loadStoryboardCache() {
	cacheDir, err := ensureSubDir(rootDir, "storyboard")
	if err != nil {
		logMsg(fmt.Sprintf("创建故事版缓存目录失败: %v", err))
		return
	}

	for i := range videos {
		v := &videos[i]
		cachePath := filepath.Join(cacheDir, "storyboard_"+v.BaseName+".jpg")

		cacheInfo, err := os.Stat(cachePath)
		if err != nil {
			storyboardQueue <- v.RelPath
			continue
		}
		videoInfo, err := os.Stat(v.AbsPath)
		if err != nil {
			storyboardQueue <- v.RelPath
			continue
		}
		// 1 秒容差：缓存 mtime + 1s >= 视频 mtime 视为命中
		if cacheInfo.ModTime().Add(1 * time.Second).Before(videoInfo.ModTime()) {
			logMsg("故事板缓存过期: " + v.RelPath)
			storyboardQueue <- v.RelPath
			continue
		}

		data, err := os.ReadFile(cachePath)
		if err != nil {
			storyboardQueue <- v.RelPath
			continue
		}
		storyboardsMu.Lock()
		storyboards[v.RelPath] = &Storyboard{Data: data}
		storyboardsMu.Unlock()
		logMsg("故事板缓存命中: " + v.RelPath)
	}
}

// ---------- 故事板生成 ----------

const (
	tileCols      = 5
	tileRows      = 5
	tileCount     = tileCols * tileRows // 25
	thumbWidth    = 320
	singleTimeout = 60 * time.Second
	jpegQuality   = 85
)

// generateStoryboard 对 25 个时间点分别做输入 seek 快速截图, 再用标准库拼成 5x5 故事版
// 原始方案需要解码整个视频并丢弃大部分结果, 现在只需要几十次快速定位并截图, 大幅提升了故事版生成速度
func generateStoryboard(v *Video) ([]byte, error) {
	dur, err := getDuration(v.AbsPath)
	if err != nil {
		return nil, err
	}

	interval := dur / float64(tileCount+1)
	if interval < 1.0 {
		interval = 1.0
	}

	// 5x5 需要 25 个不同时间点；短视频无法覆盖时明确报错，避免逐帧失败后才发现
	if interval*float64(tileCount) >= dur {
		return nil, fmt.Errorf(
			"video too short for %dx%d storyboard: duration %.3fs, need > %.3fs",
			tileCols, tileRows, dur, interval*float64(tileCount),
		)
	}

	var dst *image.RGBA
	var baseW, baseH int

	// 串行截取 25 帧，逐张绘制到大图，避免保留 25 张中间图
	for i := 1; i <= tileCount; i++ {
		ts := interval * float64(i)
		img, err := captureFrame(v.AbsPath, ts)
		if err != nil {
			return nil, fmt.Errorf("capture frame %d/%d at %.3fs: %w", i, tileCount, ts, err)
		}

		b := img.Bounds()
		if i == 1 {
			baseW, baseH = b.Dx(), b.Dy()
			dst = image.NewRGBA(image.Rect(0, 0, baseW*tileCols, baseH*tileRows))
		} else if b.Dx() != baseW || b.Dy() != baseH {
			return nil, fmt.Errorf(
				"frame %d size %dx%d differs from base %dx%d",
				i, b.Dx(), b.Dy(), baseW, baseH,
			)
		}

		col := (i - 1) % tileCols
		row := (i - 1) / tileCols
		rect := image.Rect(col*baseW, row*baseH, (col+1)*baseW, (row+1)*baseH)
		draw.Draw(dst, rect, img, img.Bounds().Min, draw.Src)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("jpeg encode: %w", err)
	}
	return buf.Bytes(), nil
}

// captureFrame 用输入 seek + -noaccurate_seek 快速截取 ts 附近的关键帧
func captureFrame(path string, ts float64) (image.Image, error) {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-ss", fmt.Sprintf("%.3f", ts),
		"-noaccurate_seek", // 输入选项：直接落在目标时间之前最近的关键帧
		"-i", path,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", thumbWidth),
		"-q:v", "3",
		"-f", "mjpeg",
		"-",
	}

	out, stderr, err := runFFmpeg(singleTimeout, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, truncateStr(stderr, 1024))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty output: %s", truncateStr(stderr, 1024))
	}

	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("jpeg decode: %w", err)
	}
	return img, nil
}

func storyboardWorker() {
	cacheDir, err := ensureSubDir(rootDir, "storyboard")
	if err != nil {
		logMsg(fmt.Sprintf("创建故事版缓存目录失败: %v", err))
		return
	}

	for relPath := range storyboardQueue {
		storyboardsMu.RLock()
		_, exists := storyboards[relPath]
		storyboardsMu.RUnlock()
		if exists {
			continue
		}

		videosMu.RLock()
		var v *Video
		for i := range videos {
			if videos[i].RelPath == relPath {
				v = &videos[i]
				break
			}
		}
		videosMu.RUnlock()
		if v == nil {
			continue
		}

		fmt.Printf("%s 开始生成故事板: %s\n", time.Now().Format("15:04:05"), relPath)
		data, err := generateStoryboard(v)
		cachePath := filepath.Join(cacheDir, "storyboard_"+v.BaseName+".jpg")
		if err != nil {
			fmt.Printf("%s 故事板生成失败: %s: %v\n", time.Now().Format("15:04:05"), relPath, err)
			// 修改点：生成失败时删除旧缓存，强制下次启动重建
			if rmErr := os.Remove(cachePath); rmErr == nil {
				logMsg("已删除过期故事板缓存: " + cachePath)
			}
			storyboardsMu.Lock()
			storyboards[relPath] = &Storyboard{Err: err}
			storyboardsMu.Unlock()
			continue
		}

		if err := os.WriteFile(cachePath, data, 0644); err != nil {
			fmt.Printf("%s 故事板缓存写入失败: %s: %v\n", time.Now().Format("15:04:05"), cachePath, err)
		}

		storyboardsMu.Lock()
		storyboards[relPath] = &Storyboard{Data: data}
		storyboardsMu.Unlock()
		fmt.Printf("%s 故事板生成完成: %s\n", time.Now().Format("15:04:05"), relPath)
	}
}

// ---------- HTTP Handlers ----------

func handleVideos(w http.ResponseWriter, r *http.Request) {
	videosMu.RLock()
	defer videosMu.RUnlock()
	json.NewEncoder(w).Encode(videos)
}

func handleDuration(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	whitelistMu.RLock()
	absPath, ok := whitelist[relPath]
	whitelistMu.RUnlock()
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	dur, err := getDuration(absPath)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	logMsg("切换文件: " + relPath)
	json.NewEncoder(w).Encode(map[string]float64{"duration": dur})
}

func handleScreenshot(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	t, err := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
	if err != nil {
		http.Error(w, "invalid t", 400)
		return
	}
	whitelistMu.RLock()
	absPath, ok := whitelist[relPath]
	whitelistMu.RUnlock()
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	screenshotMu.Lock()
	defer screenshotMu.Unlock()

	out, stderr, err := runFFmpeg(30*time.Second,
		"-hide_banner",
		"-ss", fmt.Sprintf("%.3f", t),
		"-i", absPath,
		"-frames:v", "1",
		"-f", "mjpeg",
		"-")
	if err != nil {
		logMsg(fmt.Sprintf("截图失败: %s t=%.3f: %v: %s", relPath, t, err, truncateStr(stderr, 2048)))
		http.Error(w, "screenshot failed", 500)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(out)
}

func handleStoryboard(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	storyboardsMu.RLock()
	sb, ok := storyboards[relPath]
	storyboardsMu.RUnlock()
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if sb.Err != nil {
		http.Error(w, sb.Err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(sb.Data)
}

// 新增：精灵图（10 帧 = 10 秒内容，5x2 拼图，单帧 960 宽）
func handleSprite(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	startT, err := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
	if err != nil || startT < 0 {
		http.Error(w, "invalid t", 400)
		return
	}
	whitelistMu.RLock()
	absPath, ok := whitelist[relPath]
	whitelistMu.RUnlock()
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	vf := "fps=1,scale=960:-2,tile=5x2"
	out, stderr, err := runFFmpeg(2*time.Minute,
		"-hide_banner",
		"-ss", fmt.Sprintf("%.3f", startT),
		"-t", "10",
		"-i", absPath,
		"-vf", vf,
		"-frames:v", "1",
		"-f", "mjpeg",
		"-")
	if err != nil || len(out) == 0 {
		logMsg(fmt.Sprintf("精灵图生成失败: %s t=%.1f: %v: %s", relPath, startT, err, truncateStr(stderr, 2048)))
		http.Error(w, "sprite failed", 500)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(out)
}

func handleClip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Path  string  `json:"path"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.End <= req.Start {
		http.Error(w, "end must > start", 400)
		return
	}
	whitelistMu.RLock()
	absPath, ok := whitelist[req.Path]
	whitelistMu.RUnlock()
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	if !clipMu.TryLock() {
		http.Error(w, "clip in progress", http.StatusConflict)
		return
	}
	defer clipMu.Unlock()

	videosMu.RLock()
	var v *Video
	for i := range videos {
		if videos[i].RelPath == req.Path {
			v = &videos[i]
			break
		}
	}
	videosMu.RUnlock()
	if v == nil {
		http.Error(w, "not found", 404)
		return
	}

	matches, _ := filepath.Glob(filepath.Join(rootDir, v.BaseName+"_clip_*"+v.Ext))
	maxN := 0
	prefix := v.BaseName + "_clip_"
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasPrefix(base, prefix) && strings.HasSuffix(base, v.Ext) {
			numStr := strings.TrimSuffix(strings.TrimPrefix(base, prefix), v.Ext)
			if n, err := strconv.Atoi(numStr); err == nil && n > maxN {
				maxN = n
			}
		}
	}
	nextN := maxN + 1
	outName := fmt.Sprintf("%s_clip_%03d%s", v.BaseName, nextN, v.Ext)
	outPath := filepath.Join(rootDir, outName)

	logMsg(fmt.Sprintf("切割开始: %s -> %s", req.Path, outName))
	_, stderr, err := runFFmpeg(30*time.Minute,
		"-hide_banner",
		"-ss", fmt.Sprintf("%.3f", req.Start),
		"-i", absPath,
		"-t", fmt.Sprintf("%.3f", req.End-req.Start),
		"-c", "copy",
		outPath)
	if err != nil {
		logMsg(fmt.Sprintf("切割失败: %s -> %s: %v: %s", req.Path, outName, err, truncateStr(stderr, 2048)))
		http.Error(w, err.Error(), 500)
		return
	}
	logMsg(fmt.Sprintf("切割成功: %s -> %s", req.Path, outName))
	json.NewEncoder(w).Encode(map[string]string{"output": outName})
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	whitelistMu.Lock()
	absPath, ok := whitelist[req.Path]
	if ok {
		delete(whitelist, req.Path)
	}
	whitelistMu.Unlock()
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	videosMu.Lock()
	for i, v := range videos {
		if v.RelPath == req.Path {
			videos = append(videos[:i], videos[i+1:]...)
			break
		}
	}
	videosMu.Unlock()

	os.Remove(absPath)
	base := filepath.Base(req.Path)
	ext := filepath.Ext(base)
	baseNoExt := strings.TrimSuffix(base, ext)
	os.Remove(filepath.Join(rootDir, "storyboard_"+baseNoExt+".jpg"))

	storyboardsMu.Lock()
	delete(storyboards, req.Path)
	storyboardsMu.Unlock()

	logMsg("删除文件: " + req.Path)
	w.WriteHeader(http.StatusOK)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimPrefix(r.URL.Path, "/download/")
	whitelistMu.RLock()
	absPath, ok := whitelist[relPath]
	whitelistMu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	fname := filepath.Base(relPath)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+fname+"\"")
	http.ServeFile(w, r, absPath)
	logMsg("下载: " + relPath)
}

// ---------- 主函数 ----------

func main() {
	flag.StringVar(&rootDir, "root", ".", "视频根目录")
	flag.IntVar(&port, "port", 8080, "端口")
	flag.Int64Var(&minSize, "min-size", 50, "最小文件大小 (MiB)")
	flag.Parse()

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		log.Fatal(err)
	}
	rootDir = absRoot

	minSize = minSize * 1024 * 1024

	storyboards = make(map[string]*Storyboard)
	storyboardQueue = make(chan string, 100)

	scanVideos()
	logMsg(fmt.Sprintf("启动完成，共 %d 个视频，根目录: %s", len(videos), rootDir))

	for i := 0; i < storyboardWorkers; i++ {
		go storyboardWorker()
	}

	loadStoryboardCache()

	http.HandleFunc("/api/videos", handleVideos)
	http.HandleFunc("/api/duration", handleDuration)
	http.HandleFunc("/api/screenshot", handleScreenshot)
	http.HandleFunc("/api/storyboard", handleStoryboard)
	http.HandleFunc("/api/sprite", handleSprite) // 新增
	http.HandleFunc("/api/clip", handleClip)
	http.HandleFunc("/api/delete", handleDelete)
	http.HandleFunc("/download/", handleDownload)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, htmlPage)
	})

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	fmt.Printf("服务启动: http://%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
