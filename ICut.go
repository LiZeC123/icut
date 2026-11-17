package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
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

// 修改点：加 mtime 判定（1 秒容差），缓存过期则重新入队
func loadStoryboardCache() {
	for i := range videos {
		v := &videos[i]
		cachePath := filepath.Join(rootDir, "storyboard_"+v.BaseName+".jpg")

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

func generateStoryboard(v *Video) ([]byte, error) {
	dur, err := getDuration(v.AbsPath)
	if err != nil {
		return nil, err
	}
	interval := dur / 26.0
	if interval < 1.0 {
		interval = 1.0
	}
	startTime := interval

	vf := fmt.Sprintf("fps=1/%.6f,scale=320:-2,tile=5x5", interval)
	baseArgs := []string{
		"-hide_banner",
		"-ss", fmt.Sprintf("%.3f", startTime),
		"-i", v.AbsPath,
		"-vf", vf,
		"-frames:v", "1",
		"-f", "mjpeg",
		"-",
	}

	skipArgs := append([]string{"-skip_frame", "nokey"}, baseArgs...)
	out, stderr, err := runFFmpeg(15*time.Minute, skipArgs...)
	if err != nil || len(out) == 0 {
		out, stderr, err = runFFmpeg(30*time.Minute, baseArgs...)
		if err != nil {
			return nil, fmt.Errorf("%v: %s", err, truncateStr(stderr, 2048))
		}
	}
	return out, nil
}

func storyboardWorker() {
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
		cachePath := filepath.Join(rootDir, "storyboard_"+v.BaseName+".jpg")
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
	flag.Int64Var(&minSize, "min-size", 300, "最小文件大小 (MiB)")
	flag.Parse()

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		log.Fatal(err)
	}
	rootDir = absRoot

	minSize = minSize * 1024 * 1024

	storyboards = make(map[string]*Storyboard)
	storyboardQueue = make(chan string, 10000)

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
