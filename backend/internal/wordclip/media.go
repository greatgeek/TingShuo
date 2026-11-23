package wordclip

// 原声片段媒体转发：<audio>/<img> 带不了 Authorization，且部分网络环境直连
// cdn.nadeshiko.co 被拦（2026-11-23 用户报障）。按 segment_id 从 word_clip 取
// 回源地址，首次下载落盘 MEDIA_DIR/clips，之后同源直出；缓存名取回源 URL 哈希，
// 片段重切后地址变化即自动走新文件。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greatgeek/TingShuo/backend/internal/httpx"
)

const (
	clipMediaMaxBytes = 40 << 20 // 片段视频一般 <10MB，超限视为异常
	clipMediaTimeout  = 90 * time.Second
)

var clipMediaClient = &http.Client{Timeout: clipMediaTimeout}

// 首访同一片段可能有并发的 Range 请求，用键锁避免重复回源。
var clipMediaLocks sync.Map

var clipMediaTypes = map[string]string{
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".mp4":  "video/mp4",
	".webp": "image/webp",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
}

// SetMediaRoot 指定缓存根目录（生产 MEDIA_DIR，本地 web/media）；为空则禁用转发。
func (h *Handler) SetMediaRoot(root string) { h.mediaRoot = root }

// Media GET /clipmedia/{kind}/{seg} —— 原声片段媒体（audio/image/video）同源缓存转发。
// 公开访问（媒体标签带不了 Authorization），但只放行 word_clip 里真实存在的片段，
// 不是开放代理。
func (h *Handler) Media(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	segID := chi.URLParam(r, "seg")
	var col string
	switch kind {
	case "audio":
		col = "audio_url"
	case "image":
		col = "image_url"
	case "video":
		col = "video_url"
	default:
		httpx.Err(w, httpx.BadRequest("BAD_KIND", "不支持的媒体类型"))
		return
	}
	if h.mediaRoot == "" {
		httpx.Err(w, httpx.NewError(http.StatusServiceUnavailable, "MEDIA_DISABLED", "媒体目录未配置"))
		return
	}
	var upstream string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT `+col+` FROM word_clip WHERE segment_id = $1 AND `+col+` <> '' LIMIT 1`,
		segID).Scan(&upstream); err != nil {
		httpx.Err(w, httpx.NotFound("片段不存在"))
		return
	}
	dest, ctype, err := h.clipCachePath(kind, upstream)
	if err != nil {
		log.Printf("wordclip media 回源地址异常 %q: %v", upstream, err)
		httpx.Err(w, httpx.BadGateway("片段媒体获取失败"))
		return
	}
	if _, err := os.Stat(dest); err != nil {
		if err := h.downloadClip(r.Context(), upstream, dest); err != nil {
			log.Printf("wordclip media 回源失败 %s: %v", upstream, err)
			httpx.Err(w, httpx.BadGateway("片段媒体获取失败"))
			return
		}
	}
	f, err := os.Open(dest)
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		httpx.Err(w, httpx.Internal(err.Error()))
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(dest), st.ModTime(), f)
}

// clipMediaPath 生成前端使用的同源地址；?v= 取回源 URL 哈希前 8 位，
// 片段重切后 URL 变化，浏览器 immutable 缓存也会随之换键。
func clipMediaPath(kind, segID, upstream string) string {
	sum := sha1.Sum([]byte(upstream))
	return "/clipmedia/" + kind + "/" + url.PathEscape(segID) +
		"?v=" + hex.EncodeToString(sum[:4])
}

func (h *Handler) clipCachePath(kind, upstream string) (string, string, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("not an absolute url")
	}
	ext := strings.ToLower(path.Ext(u.Path))
	ctype, ok := clipMediaTypes[ext]
	if !ok {
		switch kind {
		case "audio":
			ext, ctype = ".mp3", "audio/mpeg"
		case "image":
			ext, ctype = ".webp", "image/webp"
		default:
			ext, ctype = ".mp4", "video/mp4"
		}
	}
	sum := sha1.Sum([]byte(upstream))
	name := hex.EncodeToString(sum[:]) + ext
	return filepath.Join(h.mediaRoot, "clips", kind, name), ctype, nil
}

func (h *Handler) downloadClip(ctx context.Context, upstream, dest string) error {
	muAny, _ := clipMediaLocks.LoadOrStore(dest, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(dest); err == nil {
		return nil // 等锁期间已被其他请求下好
	}
	body, err := fetchClip(ctx, upstream)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(body, clipMediaMaxBytes+1))
	if err != nil {
		return err
	}
	if n > clipMediaMaxBytes {
		return fmt.Errorf("文件超过 %dMB 上限", clipMediaMaxBytes>>20)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		if _, stErr := os.Stat(dest); stErr == nil {
			os.Remove(tmpName) // Windows 下目标已存在会拒绝覆盖
			tmpName = ""
			return nil
		}
		return err
	}
	tmpName = ""
	return nil
}

// fetchClip 回源一次重试：ECS 到 Cloudflare 偶发冷启动慢或断连。
func fetchClip(ctx context.Context, upstream string) (io.ReadCloser, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
		if err != nil {
			return nil, err
		}
		resp, err := clipMediaClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp.Body, nil
		}
		resp.Body.Close()
		lastErr = fmt.Errorf("回源 HTTP %d", resp.StatusCode)
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests &&
			resp.StatusCode != http.StatusRequestTimeout {
			break
		}
	}
	return nil, lastErr
}
