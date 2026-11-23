package podcast

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/greatgeek/TingShuo/backend/internal/httpx"
)

// MediaRoot 是本课播客/听力等静态媒体的根目录（URL /media/ 对应这里）。
// 生产用 MEDIA_DIR 指向 /opt/tingshuo/media，deploy.sh 换 src 时不会被清掉；
// 本地默认仓库的 web/media。
func MediaRoot() string {
	if v := strings.TrimSpace(os.Getenv("MEDIA_DIR")); v != "" {
		return v
	}
	for _, dir := range []string{"../web/media", "web/media"} {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return ""
}

var lessonAudioRe = regexp.MustCompile(`^(word|grammar)-([0-9]{1,3})\.(m4a|mp3|wav|ogg|aac)$`)

type lessonAudioItem struct {
	Kind   string `json:"kind"`
	Lesson int    `json:"lesson"`
	URL    string `json:"url"`
}

// LessonAudio 列出 media/podcast 下已就绪的本课播客，前端据此决定按钮显隐。
// 命名约定：生词 第13课.m4a → word-13.m4a，语法 → grammar-13.m4a（tools/podcast 同步）。
func (h *Handler) LessonAudio(w http.ResponseWriter, r *http.Request) {
	items := []lessonAudioItem{}
	root := MediaRoot()
	if root != "" {
		entries, err := os.ReadDir(filepath.Join(root, "podcast"))
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				m := lessonAudioRe.FindStringSubmatch(strings.ToLower(e.Name()))
				if m == nil {
					continue
				}
				no, err := strconv.Atoi(m[2])
				if err != nil {
					continue
				}
				items = append(items, lessonAudioItem{
					Kind:   m[1],
					Lesson: no,
					URL:    "/media/podcast/" + url.PathEscape(e.Name()),
				})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Lesson < items[j].Lesson
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}