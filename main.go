// KTV 局域网点歌系统服务端（Go 版）
//
// 由原 Node.js 版（express + better-sqlite3 + ws）完整重写，API 行为
// 与原版保持一致：
//   - 曲库管理：管理员密码首次设置/登录/改密码（内存 session + httpOnly cookie）
//   - 曲库：歌曲列表/首字母/歌手/历史/榜单/收藏/扫描/统计
//   - 点歌队列：点歌/置顶/删除/切歌，WebSocket 实时广播
//   - 播放：HLS 渐进式转码（多音轨原唱/伴唱、VAAPI 硬件加速、缓存每日清理）
//   - 源文件直传兜底（Range 支持）
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ktvhome/internal/admin"
	"ktvhome/internal/config"
	"ktvhome/internal/db"
	"ktvhome/internal/hls"
	"ktvhome/internal/logger"
	"ktvhome/internal/scanner"
	"ktvhome/internal/ws"
)

var (
	cfg *config.Config
	adm *admin.Admin
	h   *hls.HLS
	sc  *scanner.Scanner
	hub *ws.Hub

	webDir string

	safeFileRe = regexp.MustCompile(`^[\w.-]+$`)
	digitsRe   = regexp.MustCompile(`^\d+$`)
)

func main() {
	cfg = config.Load()
	cfg.Resolve()

	// 确保数据目录与封面目录存在。
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "covers")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			logger.Error("SERVER", "创建数据目录失败: "+err.Error())
			os.Exit(1)
		}
	}

	if err := db.Init(cfg.DataDir); err != nil {
		logger.Error("SERVER", err.Error())
		os.Exit(1)
	}
	defer db.DB.Close()

	webDir = findWebDir()

	adm = admin.New()
	// 1.2.0：ADMIN_PASSWORD 环境变量预置管理员密码（仅首次未设密码时生效）。
	adm.EnsureInitialPassword(cfg.AdminPassword)

	h = hls.New(cfg.HLSDir, cfg.VAAPIDevice, cfg.HLSCacheMaxAgeDays, cfg.NVENCEnabled)
	h.EnsureDir()
	sc = &scanner.Scanner{MVDir: cfg.MVDir, MVNetDir: cfg.MVNetDir, RemoveHLS: h.Remove}
	hub = ws.NewHub()
	hub.GetQueue = queuePayload

	mux := http.NewServeMux()
	registerRoutes(mux)

	srv := &http.Server{
		Addr:    "0.0.0.0:" + cfg.Port,
		Handler: mux,
	}

	// 启动扫描挪到监听之后异步触发：端口立刻开始监听，曲库随扫描推进逐步变长，
	// 用户不需要等整轮扫描跑完才看到歌曲。
	go func() {
		result := sc.ScanLibrary()
		if result.Error != "" {
			logger.Error("SCAN", "初始扫描失败: "+result.Error)
		} else {
			logger.Info("SCAN", fmt.Sprintf("初始扫描完成: 共%d首，新增%d首，移除%d首", result.Total, result.Added, result.Removed))
		}
	}()

	// HLS 缓存每日清理（每次触发时取当次最新的曲库状态判断）。
	h.ScheduleCleanup(func() []int64 {
		rows, err := db.DB.Query("SELECT id FROM songs")
		if err != nil {
			return nil
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		return ids
	})

	// 预热 VAAPI 自检，避免第一首点歌额外等待。
	go h.Warmup()

	logger.Info("SERVER", fmt.Sprintf("KTV 服务已启动: http://0.0.0.0:%s (web=%s)", cfg.Port, webDir))
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("SERVER", "HTTP 服务退出: "+err.Error())
	}
}

// findWebDir 定位前端静态资源目录：优先环境变量，其次可执行文件相对路径，
// 最后回退到工作目录下的 web。
func findWebDir() string {
	if v := os.Getenv("WEB_DIR"); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		for _, c := range []string{
			filepath.Join(base, "web"),
			filepath.Join(base, "..", "web"),
			filepath.Join(base, "..", "..", "web"),
		} {
			if st, err := os.Stat(c); err == nil && st.IsDir() {
				return c
			}
		}
	}
	return "web"
}

func registerRoutes(mux *http.ServeMux) {
	// ---------- 曲库管理 ----------
	mux.HandleFunc("GET /api/admin/session", adm.SessionStatus)
	mux.HandleFunc("POST /api/admin/setup", adm.Setup)
	mux.HandleFunc("POST /api/admin/login", adm.Login)
	mux.HandleFunc("POST /api/admin/logout", adm.Logout)
	mux.HandleFunc("POST /api/admin/change-password", adm.RequireAuth(adm.ChangePassword))

	// ---------- 歌曲库 ----------
	mux.HandleFunc("GET /api/songs", handleSongs)
	mux.HandleFunc("GET /api/songs/letter/{letter}", handleSongsLetter)
	mux.HandleFunc("DELETE /api/songs/{id}", adm.RequireAuth(handleDeleteSong))
	mux.HandleFunc("PUT /api/songs/{id}", adm.RequireAuth(handleUpdateSong))
	// 1.2.0：批量设置语种/风格。
	mux.HandleFunc("PUT /api/songs/batch", adm.RequireAuth(handleSongsBatch))

	// ---------- 歌手 / 历史 / 榜单 / 收藏 ----------
	mux.HandleFunc("GET /api/artists", handleArtists)
	mux.HandleFunc("GET /api/history", handleHistory)
	mux.HandleFunc("GET /api/charts", handleCharts)
	mux.HandleFunc("GET /api/favorites", handleFavorites)
	mux.HandleFunc("POST /api/favorites/{song_id}", handleAddFavorite)
	mux.HandleFunc("DELETE /api/favorites/{song_id}", handleRemoveFavorite)

	// ---------- 扫描 / 统计 / 音轨切换上报 ----------
	mux.HandleFunc("POST /api/scan", handleScan)
	mux.HandleFunc("GET /api/stats", handleStats)
	mux.HandleFunc("POST /api/voice/switch", handleVoiceSwitch)

	// ---------- 语种/风格预设（1.2.0：后台可自定义下拉选项） ----------
	mux.HandleFunc("GET /api/admin/presets", adm.RequireAuth(handlePresetsGet))
	mux.HandleFunc("POST /api/admin/presets", adm.RequireAuth(handlePresetsAdd))
	mux.HandleFunc("DELETE /api/admin/presets", adm.RequireAuth(handlePresetsDelete))

	// ---------- 曲库来源管理（1.2.0 多曲库：/mv 本地 + /mv-net 网盘） ----------
	mux.HandleFunc("GET /api/library-sources", adm.RequireAuth(handleLibrarySources))
	mux.HandleFunc("POST /api/library-sources/{name}", adm.RequireAuth(handleLibrarySourceSet))

	// ---------- 点歌队列 ----------
	mux.HandleFunc("GET /api/queue", handleQueueGet)
	mux.HandleFunc("POST /api/queue", handleQueueAdd)
	mux.HandleFunc("POST /api/queue/next", handleQueueNext)
	mux.HandleFunc("POST /api/queue/{id}/top", handleQueueTop)
	mux.HandleFunc("DELETE /api/queue/{id}", handleQueueDelete)

	// ---------- 播放器状态/设置（安卓 TV 预留接口） ----------
	// GET /api/player/state    当前播放/下一曲/队列/设置（安卓轮询用）
	// GET/PUT /api/player/settings  播放器设置（含播完随机播放开关）
	mux.HandleFunc("GET /api/player/state", handlePlayerState)
	mux.HandleFunc("GET /api/player/settings", handlePlayerSettingsGet)
	mux.HandleFunc("PUT /api/player/settings", handlePlayerSettingsSet)

	// ---------- TV 滚动欢迎语 ----------
	// GET /api/welcome（公开，TV 端读取）；PUT /api/admin/welcome（后台设置）
	mux.HandleFunc("GET /api/welcome", handleWelcomeGet)
	mux.HandleFunc("PUT /api/admin/welcome", adm.RequireAuth(handleWelcomeSet))

	// ---------- HLS 播放 ----------
	mux.HandleFunc("GET /hls/{id}/master.m3u8", handleHLSMaster)
	mux.HandleFunc("GET /hls/{id}/{file}", handleHLSFile)

	// ---------- MV 直传兜底 ----------
	mux.HandleFunc("GET /stream/{id}", handleStream)

	// ---------- 静态资源 ----------
	mux.Handle("GET /tv/", http.StripPrefix("/tv/", http.FileServer(http.Dir(filepath.Join(webDir, "tv")))))
	mux.Handle("GET /m/", http.StripPrefix("/m/", http.FileServer(http.Dir(filepath.Join(webDir, "mobile")))))
	mux.Handle("GET /admin/", http.StripPrefix("/admin/", http.FileServer(http.Dir(filepath.Join(webDir, "admin")))))
	// 1.2.0：PAD（平板）点歌页。
	mux.Handle("GET /pad/", http.StripPrefix("/pad/", http.FileServer(http.Dir(filepath.Join(webDir, "pad")))))
	mux.Handle("GET /cover/", http.StripPrefix("/cover/", http.FileServer(http.Dir(filepath.Join(cfg.DataDir, "covers")))))
	// 1.2.0：歌手头像目录（图片名=歌手名，如 周杰伦.jpg），前端按歌手名拼接 URL。
	mux.Handle("GET /singer/", http.StripPrefix("/singer/", http.FileServer(http.Dir(cfg.SingerDir))))
	// 主页：根路径展示 TV / 手机 / 管理三个入口（web/index.html）。
	mux.Handle("GET /", http.FileServer(http.Dir(webDir)))

	// ---------- WebSocket ----------
	mux.HandleFunc("GET /ws", hub.Handle)
}

// ---------- 通用工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, v)
}

func nullStr(v sql.NullString) *string {
	if v.Valid {
		s := v.String
		return &s
	}
	return nil
}

func nullInt(v sql.NullInt64) *int64 {
	if v.Valid {
		i := v.Int64
		return &i
	}
	return nil
}

// scanSong 按 songs 表列序扫描一行（必须配合显式列名查询，见 songCols）。
// created_at 由驱动解析为 time.Time，此处统一格式化为 SQLite 原始文本
// （"YYYY-MM-DD HH:MM:SS"），与原版 better-sqlite3 返回格式保持一致。
func scanSong(scan func(dest ...any) error) (db.Song, error) {
	var s db.Song
	var artist, cover, pinyin, language, genre sql.NullString
	var duration, audioTracks sql.NullInt64
	var createdAt any
	err := scan(
		&s.ID, &s.Title, &artist, &s.Filename, &s.Filepath, &cover,
		&duration, &pinyin, &s.PlayCount, &audioTracks, &language, &genre, &createdAt,
	)
	if err != nil {
		return s, err
	}
	s.Artist = nullStr(artist)
	s.Cover = nullStr(cover)
	s.Pinyin = nullStr(pinyin)
	s.Duration = nullInt(duration)
	s.AudioTracks = nullInt(audioTracks)
	s.Language = nullStr(language)
	s.Genre = nullStr(genre)
	s.CreatedAt = formatTimeValue(createdAt)
	return s, nil
}

// songCols / songColsPlain：scanSong 按位置扫描，所有查询必须显式列名，
// 且列顺序与 scanSong 严格一致。老库经 ALTER TABLE 补 language/genre 列后
// 物理列序不同（created_at 在 language/genre 之前），用 s.* 会导致错位。
const (
	songCols      = "s.id, s.title, s.artist, s.filename, s.filepath, s.cover, s.duration, s.pinyin, s.play_count, s.audio_tracks, s.language, s.genre, s.created_at"
	songColsPlain = "id, title, artist, filename, filepath, cover, duration, pinyin, play_count, audio_tracks, language, genre, created_at"
)

// formatTimeValue 将驱动返回的时间值统一转为 "YYYY-MM-DD HH:MM:SS" 文本。
func formatTimeValue(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.Format("2006-01-02 15:04:05")
	case string:
		return t
	case []byte:
		return string(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func getSongByID(id int64) (*db.Song, error) {
	row := db.DB.QueryRow("SELECT "+songColsPlain+" FROM songs WHERE id = ?", id)
	s, err := scanSong(row.Scan)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---------- 歌曲库 ----------

// handleSongs 歌曲列表：
//   - ?q= 关键字（歌名/歌手模糊）
//   - ?artist= 指定歌手（匹配 song_artists 拆分后的任一歌手）
//   - ?incomplete=any|artist|language|genre|track 信息不完整筛选（管理后台用）
//   - ?scope=local|network 本地/网络曲库筛选
//   - ?page= / ?pageSize= 分页（管理后台每页 50 首）
//
// 无分页参数时返回完整列表（兼容 TV/手机端旧行为）。
func handleSongs(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	incomplete := strings.TrimSpace(r.URL.Query().Get("incomplete"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	pageStr := r.URL.Query().Get("page")
	pageSizeStr := r.URL.Query().Get("pageSize")

	where := []string{"1=1"}
	args := []any{}
	if q != "" {
		where = append(where, "(title LIKE ? OR artist LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if artist != "" {
		// 按拆分后歌手匹配：通过 song_artists 关联表。
		where = append(where, "s.id IN (SELECT song_id FROM song_artists WHERE artist = ?)")
		args = append(args, artist)
	}
	switch incomplete {
	case "any":
		where = append(where, "(artist IS NULL OR artist='' OR language IS NULL OR language='' OR genre IS NULL OR genre='')")
	case "artist":
		where = append(where, "(artist IS NULL OR artist='')")
	case "language":
		where = append(where, "(language IS NULL OR language='')")
	case "genre":
		where = append(where, "(genre IS NULL OR genre='')")
	case "track":
		where = append(where, "(audio_tracks IS NULL OR audio_tracks < 2)")
	}
	switch scope {
	case "local":
		where = append(where, "s.filename NOT LIKE 'net/%' AND s.filename NOT LIKE 'net_net/%'")
	case "network":
		where = append(where, "(s.filename LIKE 'net/%' OR s.filename LIKE 'net_net/%')")
	}
	whereSQL := strings.Join(where, " AND ")

	paged := pageStr != ""
	var page, pageSize int64 = 1, 50
	if paged {
		page, _ = strconv.ParseInt(pageStr, 10, 64)
		if v, err := strconv.ParseInt(pageSizeStr, 10, 64); err == nil && v > 0 {
			pageSize = v
		}
		if page < 1 {
			page = 1
		}
	}

	var total int64
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM songs s WHERE "+whereSQL, args...).Scan(&total)

	sqlTail := " ORDER BY s.play_count DESC, s.id DESC"
	if paged {
		sqlTail += fmt.Sprintf(" LIMIT %d OFFSET %d", pageSize, (page-1)*pageSize)
	} else {
		sqlTail += " LIMIT 500"
	}

	rows, err := db.DB.Query("SELECT "+songCols+" FROM songs s WHERE "+whereSQL+sqlTail, args...)
	if err != nil {
		logger.Error("API", "查询歌曲列表失败: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	defer rows.Close()

	songs := make([]db.Song, 0)
	for rows.Next() {
		if s, err := scanSong(rows.Scan); err == nil {
			songs = append(songs, s)
		}
	}
	if !paged {
		writeOK(w, songs)
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	writeOK(w, map[string]any{
		"items":      songs,
		"total":      total,
		"page":       page,
		"totalPages": totalPages,
	})
}

func handleSongsLetter(w http.ResponseWriter, r *http.Request) {
	letter := strings.ToUpper(r.PathValue("letter"))
	rows, err := db.DB.Query("SELECT "+songColsPlain+" FROM songs WHERE UPPER(SUBSTR(title,1,1)) = ? ORDER BY title LIMIT 100", letter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	defer rows.Close()

	songs := make([]db.Song, 0)
	for rows.Next() {
		if s, err := scanSong(rows.Scan); err == nil {
			songs = append(songs, s)
		}
	}
	writeOK(w, songs)
}

func handleDeleteSong(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	if _, err := db.DB.Exec("DELETE FROM songs WHERE id = ?", id); err != nil {
		logger.Error("API", "删除歌曲失败: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除失败"})
		return
	}
	h.Remove(id)
	writeOK(w, map[string]bool{"ok": true})
}

func handleUpdateSong(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	var body struct {
		Title    string `json:"title"`
		Artist   string `json:"artist"`
		Language string `json:"language"`
		Genre    string `json:"genre"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if _, err := db.DB.Exec(
		"UPDATE songs SET title=?, artist=?, language=?, genre=? WHERE id=?",
		body.Title, body.Artist, body.Language, body.Genre, id,
	); err != nil {
		logger.Error("API", "更新歌曲失败: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "更新失败"})
		return
	}
	// artist 可能变化（& 拆分），同步重建歌手关联表。
	scanner.RebuildSongArtists()
	writeOK(w, map[string]bool{"ok": true})
}

// handleSongsBatch 批量设置语种/风格（管理后台多选后统一修改）。
func handleSongsBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs         []int64 `json:"ids"`
		SetLanguage bool    `json:"setLanguage"`
		Language    string  `json:"language"`
		SetGenre    bool    `json:"setGenre"`
		Genre       string  `json:"genre"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if len(body.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未选择歌曲"})
		return
	}
	if !body.SetLanguage && !body.SetGenre {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "至少选择语种或风格一项"})
		return
	}
	tx, err := db.DB.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "批量更新失败"})
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("UPDATE songs SET language = CASE WHEN ? THEN ? ELSE language END, genre = CASE WHEN ? THEN ? ELSE genre END WHERE id = ?")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "批量更新失败"})
		return
	}
	defer stmt.Close()
	for _, id := range body.IDs {
		if _, err := stmt.Exec(body.SetLanguage, body.Language, body.SetGenre, body.Genre, id); err != nil {
			logger.Error("API", "批量更新歌曲失败: "+err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "批量更新失败"})
			return
		}
	}
	if err := tx.Commit(); err != nil {
		logger.Error("API", "批量更新歌曲提交失败: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "批量更新失败"})
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

// ---------- 歌手 / 历史 / 榜单 / 收藏 ----------

// handleArtists 歌手列表：从 song_artists 关联表聚合（合唱/多歌手按 &
// 拆分后每位独立可见），返回按歌名字数排序的歌手与歌曲数。
// hasAvatar 表示 /singer 目录下是否存在以歌手名命名的头像图片。
func handleArtists(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query(`
		SELECT sa.artist, COUNT(DISTINCT sa.song_id) as count
		FROM song_artists sa
		GROUP BY sa.artist ORDER BY sa.artist`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	defer rows.Close()

	type artistRow struct {
		Artist     string `json:"artist"`
		Count      int64  `json:"count"`
		HasAvatar  bool   `json:"hasAvatar"`
	}
	out := make([]artistRow, 0)
	for rows.Next() {
		var a artistRow
		if rows.Scan(&a.Artist, &a.Count) == nil {
			a.HasAvatar = singerAvatarExists(a.Artist)
			out = append(out, a)
		}
	}
	writeOK(w, out)
}

// singerAvatarExists 判断歌手头像文件（/singer/<歌手名>.jpg|png|jpeg|webp）是否存在。
func singerAvatarExists(artist string) bool {
	if cfg == nil || cfg.SingerDir == "" {
		return false
	}
	for _, ext := range []string{".jpg", ".png", ".jpeg", ".webp"} {
		if fileExists(filepath.Join(cfg.SingerDir, artist+ext)) {
			return true
		}
	}
	return false
}

// ---------- 语种/风格预设 ----------

const (
	presetLangKey  = "preset_languages"
	presetGenreKey = "preset_genres"
)

// loadPresetList 读取预设列表；为空时返回默认预设（首次使用自动写入）。
func loadPresetList(key string, defaults []string) []string {
	raw, ok, err := db.GetSetting(key)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		_ = db.SetSetting(key, strings.Join(defaults, ","))
		return defaults
	}
	var out []string
	for _, v := range strings.Split(raw, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return defaults
	}
	return out
}

func savePresetList(key string, list []string) error {
	return db.SetSetting(key, strings.Join(list, ","))
}

func handlePresetsGet(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{
		"languages": loadPresetList(presetLangKey, scanner.DefaultLanguages),
		"genres":    loadPresetList(presetGenreKey, scanner.DefaultGenres),
	})
}

func handlePresetsAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	value := strings.TrimSpace(body.Value)
	if value == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "预设内容不能为空"})
		return
	}
	var key string
	var defaults []string
	switch body.Type {
	case "language":
		key, defaults = presetLangKey, scanner.DefaultLanguages
	case "genre":
		key, defaults = presetGenreKey, scanner.DefaultGenres
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知预设类型"})
		return
	}
	list := loadPresetList(key, defaults)
	for _, v := range list {
		if v == value {
			writeOK(w, map[string]bool{"ok": true})
			return
		}
	}
	list = append(list, value)
	if err := savePresetList(key, list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败"})
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

func handlePresetsDelete(w http.ResponseWriter, r *http.Request) {
	ptype := strings.TrimSpace(r.URL.Query().Get("type"))
	value := strings.TrimSpace(r.URL.Query().Get("value"))
	if value == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少预设值"})
		return
	}
	var key string
	var defaults []string
	switch ptype {
	case "language":
		key, defaults = presetLangKey, scanner.DefaultLanguages
	case "genre":
		key, defaults = presetGenreKey, scanner.DefaultGenres
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知预设类型"})
		return
	}
	list := loadPresetList(key, defaults)
	out := list[:0]
	for _, v := range list {
		if v != value {
			out = append(out, v)
		}
	}
	if err := savePresetList(key, out); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败"})
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query(`
		SELECT `+songCols+`, COUNT(h.id) as times_sung
		FROM songs s JOIN history h ON s.id = h.song_id
		GROUP BY s.id ORDER BY times_sung DESC, s.play_count DESC LIMIT 50`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	defer rows.Close()

	out := make([]db.HistoryEntry, 0)
	for rows.Next() {
		var e db.HistoryEntry
		s, err := scanSong(func(dest ...any) error {
			return rows.Scan(append(dest, &e.TimesSung)...)
		})
		if err == nil {
			e.Song = s
			out = append(out, e)
		}
	}
	writeOK(w, out)
}

func handleCharts(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query("SELECT "+songColsPlain+" FROM songs WHERE play_count > 0 ORDER BY play_count DESC LIMIT 50")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	defer rows.Close()

	songs := make([]db.Song, 0)
	for rows.Next() {
		if s, err := scanSong(rows.Scan); err == nil {
			songs = append(songs, s)
		}
	}
	writeOK(w, songs)
}

func handleFavorites(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	if device == "" {
		device = "default"
	}
	rows, err := db.DB.Query(`
		SELECT `+songCols+` FROM songs s
		JOIN favorites f ON s.id = f.song_id
		WHERE f.device_id = ? ORDER BY f.created_at DESC`, device)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	defer rows.Close()

	songs := make([]db.Song, 0)
	for rows.Next() {
		if s, err := scanSong(rows.Scan); err == nil {
			songs = append(songs, s)
		}
	}
	writeOK(w, songs)
}

func handleAddFavorite(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("song_id"))
	if !ok {
		return
	}
	var body struct {
		Device string `json:"device"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Device == "" {
		body.Device = "default"
	}
	if _, err := db.DB.Exec("INSERT OR IGNORE INTO favorites (song_id, device_id) VALUES (?,?)", id, body.Device); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "收藏失败"})
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

func handleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("song_id"))
	if !ok {
		return
	}
	device := r.URL.Query().Get("device")
	if device == "" {
		device = "default"
	}
	if _, err := db.DB.Exec("DELETE FROM favorites WHERE song_id = ? AND device_id = ?", id, device); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除收藏失败"})
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

// ---------- 扫描 / 统计 / 音轨切换 ----------

func handleScan(w http.ResponseWriter, r *http.Request) {
	result := sc.ScanLibrary()
	writeOK(w, map[string]any{
		"ok":      result.Error == "",
		"total":   result.Total,
		"added":   result.Added,
		"removed": result.Removed,
		"error":   result.Error,
	})
}

// ---------- 曲库来源管理（1.2.0 多曲库） ----------

// handleLibrarySources 返回全部曲库来源及其启用状态（后台「曲库来源」页面）。
func handleLibrarySources(w http.ResponseWriter, r *http.Request) {
	sources := sc.DiscoverSources()
	if sources == nil {
		sources = []scanner.Source{}
	}
	writeOK(w, sources)
}

// handleLibrarySourceSet 设置某个曲库来源的启用状态。
func handleLibrarySourceSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少曲库来源名"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := scanner.EnableSource(name, body.Enabled); err != nil {
		logger.Error("API", "设置曲库来源状态失败("+name+"): "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败"})
		return
	}
	logger.Info("SCAN", fmt.Sprintf("曲库来源「%s」已%s", name, map[bool]string{true: "启用", false: "禁用"}[body.Enabled]))
	writeOK(w, map[string]bool{"ok": true})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	var songCount, queueCount, totalPlays int64
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM songs").Scan(&songCount)
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM queue WHERE status!='done'").Scan(&queueCount)
	_ = db.DB.QueryRow("SELECT COALESCE(SUM(play_count),0) FROM songs").Scan(&totalPlays)
	writeOK(w, map[string]any{
		"songCount":  songCount,
		"queueCount": queueCount,
		"totalPlays": totalPlays,
		"mvDir":      cfg.MVDir,
	})
}

func handleVoiceSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SongID any    `json:"song_id"`
		Mode   string `json:"mode"`
		To     string `json:"to"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	var songTag string
	switch v := body.SongID.(type) {
	case float64:
		id := int64(v)
		if song, err := getSongByID(id); err == nil {
			songTag = fmt.Sprintf(`id=%d "%s"`, song.ID, titleOrFilename(song))
		} else {
			songTag = fmt.Sprintf("id=%d", id)
		}
	default:
		songTag = "id=未知"
	}

	toName := "未知"
	switch body.To {
	case "original":
		toName = "原唱"
	case "accompaniment":
		toName = "伴唱"
	}
	modeName := "未知"
	switch body.Mode {
	case "tracks":
		modeName = "多音轨(HLS audioTrack)"
	case "stereo":
		modeName = "双声道(Web Audio 声道复制)"
	}
	logger.Info("VOICE", fmt.Sprintf("切换音轨: %s -> %s (方式: %s)", songTag, toName, modeName))
	writeOK(w, map[string]bool{"ok": true})
}

func titleOrFilename(song *db.Song) string {
	if song.Title != "" {
		return song.Title
	}
	return song.Filename
}

// ---------- 点歌队列 ----------

// getQueueWithSongs 查询完整点歌队列（含歌曲信息）。
// 排序修复：正在播放的永远排第一，其次置顶，再按 id——置顶只能到"正在播放之后
// 的第一位"，不能把正在播放的挤出队首。
func getQueueWithSongs() []db.QueueEntry {
	rows, err := db.DB.Query(`
		SELECT q.id as queue_id, q.nickname, q.is_top, q.status, q.created_at,
		       s.id as song_id, s.title, s.artist, s.filename, s.cover, s.duration,
		       s.audio_tracks
		FROM queue q JOIN songs s ON q.song_id = s.id
		WHERE q.status != 'done'
		ORDER BY (q.status='playing') DESC, q.is_top DESC, q.id ASC`)
	if err != nil {
		logger.Error("API", "查询点歌队列失败: "+err.Error())
		return nil
	}
	defer rows.Close()

	out := make([]db.QueueEntry, 0)
	for rows.Next() {
		var e db.QueueEntry
		var artist, cover sql.NullString
		var duration, audioTracks, isTop sql.NullInt64
		var createdAt any
		err := rows.Scan(
			&e.QueueID, &e.Nickname, &isTop, &e.Status, &createdAt,
			&e.SongID, &e.Title, &artist, &e.Filename, &cover, &duration,
			&audioTracks,
		)
		if err != nil {
			continue
		}
		e.IsTop = isTop.Int64
		e.Artist = nullStr(artist)
		e.Cover = nullStr(cover)
		e.Duration = nullInt(duration)
		e.AudioTracks = nullInt(audioTracks)
		e.CreatedAt = formatTimeValue(createdAt)
		out = append(out, e)
	}
	return out
}

func queuePayload() []byte {
	payload, err := json.Marshal(map[string]any{"type": "queue", "data": getQueueWithSongs()})
	if err != nil {
		return nil
	}
	return payload
}

func broadcastQueue() {
	if payload := queuePayload(); payload != nil {
		hub.Broadcast(payload)
	}
}

func handleQueueGet(w http.ResponseWriter, r *http.Request) {
	writeOK(w, getQueueWithSongs())
}

func handleQueueAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SongID   int64  `json:"song_id"`
		Nickname string `json:"nickname"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	_, err := getSongByID(body.SongID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "歌曲不存在"})
		return
	}
	if body.Nickname == "" {
		body.Nickname = "匿名歌手"
	}

	// 重复点歌拦截：同一首歌已在播放或等待中，不重复加入队列。
	var dup int64
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM queue WHERE status IN ('playing','waiting') AND song_id=?", body.SongID).Scan(&dup)
	if dup > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这首歌已在队列中"})
		return
	}

	res, err := db.DB.Exec("INSERT INTO queue (song_id,nickname) VALUES (?,?)", body.SongID, body.Nickname)
	if err != nil {
		logger.Error("API", "点歌失败: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "点歌失败"})
		return
	}
	newID, _ := res.LastInsertId()

	_, _ = db.DB.Exec("UPDATE songs SET play_count=play_count+1 WHERE id=?", body.SongID)

	// 队列为空时，新点的歌直接进入播放状态。
	var playing int64
	err = db.DB.QueryRow("SELECT id FROM queue WHERE status='playing' LIMIT 1").Scan(&playing)
	if err == sql.ErrNoRows {
		_, _ = db.DB.Exec("UPDATE queue SET status='playing' WHERE id=?", newID)
	}

	broadcastQueue()
	writeOK(w, map[string]any{"ok": true, "id": newID})
}

func handleQueueNext(w http.ResponseWriter, r *http.Request) {
	// 当前播放 → done + 写入历史。
	var cur struct {
		ID       int64
		SongID   int64
		Nickname string
	}
	err := db.DB.QueryRow("SELECT id, song_id, nickname FROM queue WHERE status='playing' ORDER BY id LIMIT 1").
		Scan(&cur.ID, &cur.SongID, &cur.Nickname)
	if err == nil {
		_, _ = db.DB.Exec("UPDATE queue SET status='done' WHERE id=?", cur.ID)
		_, _ = db.DB.Exec("INSERT INTO history (song_id,nickname) VALUES (?,?)", cur.SongID, cur.Nickname)
	}

	// 下一首 waiting 中最靠前的（置顶优先）进入播放。
	var nxtID int64
	err = db.DB.QueryRow("SELECT id FROM queue WHERE status='waiting' ORDER BY is_top DESC, id ASC LIMIT 1").Scan(&nxtID)
	if err == nil {
		_, _ = db.DB.Exec("UPDATE queue SET status='playing' WHERE id=?", nxtID)
	} else if err == sql.ErrNoRows && playerAutoplayRandom() {
		// 队列已空且开启「播完随机播放」：随机挑一首进入播放。
		// 安卓 TV 播完调用本接口即可按设置自动续播。
		var sid int64
		if e2 := db.DB.QueryRow("SELECT id FROM songs ORDER BY RANDOM() LIMIT 1").Scan(&sid); e2 == nil {
			if res, e3 := db.DB.Exec("INSERT INTO queue (song_id, nickname, status) VALUES (?, ?, 'playing')", sid, "随机播放"); e3 == nil {
				if id, _ := res.LastInsertId(); id > 0 {
					_, _ = db.DB.Exec("UPDATE songs SET play_count=play_count+1 WHERE id=?", sid)
				}
			}
		}
	}

	broadcastQueue()
	writeOK(w, map[string]bool{"ok": true})
}

// ---------- 播放器状态 / 设置（安卓 TV 预留接口） ----------

// playerAutoplayRandomKey 播完自动随机播放开关（settings 表，默认关闭）。
const playerAutoplayRandomKey = "player_autoplay_random"

// playerAutoplayRandom 读取播完随机播放开关；未设置过默认关闭。
func playerAutoplayRandom() bool {
	v, ok, err := db.GetSetting(playerAutoplayRandomKey)
	if err != nil || !ok {
		return false
	}
	return v == "1"
}

// handlePlayerSettingsGet 返回播放器设置（安卓 TV 播完读取用）。
func handlePlayerSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]bool{"autoplay_random": playerAutoplayRandom()})
}

// handlePlayerSettingsSet 保存播放器设置。
func handlePlayerSettingsSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoplayRandom *bool `json:"autoplay_random"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.AutoplayRandom != nil {
		v := "0"
		if *body.AutoplayRandom {
			v = "1"
		}
		if err := db.SetSetting(playerAutoplayRandomKey, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存设置失败"})
			return
		}
	}
	writeOK(w, map[string]bool{"autoplay_random": playerAutoplayRandom()})
}

// handlePlayerState 返回播放端状态：正在播放、下一曲、队列与设置（安卓 TV 轮询用）。
func handlePlayerState(w http.ResponseWriter, r *http.Request) {
	type item struct {
		QueueID  int64  `json:"queue_id"`
		SongID   int64  `json:"song_id"`
		Title    string `json:"title"`
		Artist   string `json:"artist"`
		Filename string `json:"filename"`
		AudioTracks *int64 `json:"audio_tracks"`
		Status   string `json:"status"`
		Nickname string `json:"nickname"`
	}
	rows, err := db.DB.Query(`
		SELECT q.id, q.song_id, q.nickname, q.status,
		       COALESCE(s.title,''), COALESCE(s.artist,''), COALESCE(s.filename,''), s.audio_tracks
		FROM queue q LEFT JOIN songs s ON s.id = q.song_id
		WHERE q.status IN ('playing','waiting')
		ORDER BY CASE WHEN q.status='playing' THEN 0 ELSE 1 END, q.is_top DESC, q.id ASC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取队列失败"})
		return
	}
	defer rows.Close()

	var items []item
	var playing, next *item
	for rows.Next() {
		var it item
		var title, artist, filename sql.NullString
		var trk sql.NullInt64
		if rows.Scan(&it.QueueID, &it.SongID, &it.Nickname, &it.Status, &title, &artist, &filename, &trk) != nil {
			continue
		}
		it.Title = title.String
		it.Artist = artist.String
		it.Filename = filename.String
		if trk.Valid {
			v := trk.Int64
			it.AudioTracks = &v
		}
		items = append(items, it)
		if it.Status == "playing" && playing == nil {
			cp := it
			playing = &cp
		}
		if it.Status == "waiting" && next == nil {
			cp := it
			next = &cp
		}
	}
	writeOK(w, map[string]any{
		"playing":  playing,
		"next":     next,
		"queue":    items,
		"settings": map[string]bool{"autoplay_random": playerAutoplayRandom()},
	})
}

// ---------- TV 滚动欢迎语 ----------

// tickerWelcomeKey 欢迎语设置键（settings 表）。
const tickerWelcomeKey = "ticker_welcome"

// defaultWelcome 默认欢迎语。
const defaultWelcome = "家悦K歌，唱响每一刻"

// welcomeText 读取欢迎语；未设置或为空时返回默认值。
func welcomeText() string {
	v, ok, err := db.GetSetting(tickerWelcomeKey)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return defaultWelcome
	}
	return v
}

// handleWelcomeGet 返回欢迎语（TV 端滚动显示用）。
func handleWelcomeGet(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]string{"welcome": welcomeText()})
}

// handleWelcomeSet 保存欢迎语（管理后台设置）。
func handleWelcomeSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Welcome string `json:"welcome"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	v := strings.TrimSpace(body.Welcome)
	if v == "" {
		v = defaultWelcome
	}
	if err := db.SetSetting(tickerWelcomeKey, v); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存欢迎语失败"})
		return
	}
	writeOK(w, map[string]string{"welcome": welcomeText()})
}

func handleQueueTop(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	// 修复：先把所有非播放中的置顶标记清空，再置顶当前这首，保证同一时刻
	// 只有一首歌处于置顶状态，每次点击都确实生效。
	tx, err := db.DB.Begin()
	if err == nil {
		_, _ = tx.Exec("UPDATE queue SET is_top=0 WHERE status!='playing'")
		_, _ = tx.Exec("UPDATE queue SET is_top=1 WHERE id=?", id)
		if err := tx.Commit(); err != nil {
			logger.Error("API", "置顶失败: "+err.Error())
		}
	}
	broadcastQueue()
	writeOK(w, map[string]bool{"ok": true})
}

func handleQueueDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	_, _ = db.DB.Exec("DELETE FROM queue WHERE id=?", id)
	broadcastQueue()
	writeOK(w, map[string]bool{"ok": true})
}

// ---------- HLS 播放 ----------

func handleHLSMaster(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	song, err := getSongByID(id)
	if err != nil || !fileExists(song.Filepath) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	logger.Info("HLS", fmt.Sprintf("请求播放 master.m3u8: id=%d \"%s\"", song.ID, titleOrFilename(song)))

	m3u8Path, err := h.Ensure(song)
	if err != nil {
		logger.Error("HLS", fmt.Sprintf("master.m3u8 生成失败: id=%d \"%s\": %s", song.ID, song.Filename, err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	serveFileWithHeaders(w, m3u8Path, "application/vnd.apple.mpegurl", "no-store")
}

func handleHLSFile(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	file := r.PathValue("file")
	if !digitsRe.MatchString(idStr) || !safeFileRe.MatchString(file) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	p := filepath.Join(h.OutDir(id), file)

	// 双重防护：路径必须位于该歌曲的 HLS 输出目录内。
	if !strings.HasPrefix(p, h.OutDir(id)+string(os.PathSeparator)) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if !fileExists(p) {
		if err := h.WaitForFile(p, id); err != nil {
			var bf *hls.BuildFailedError
			if errors.As(err, &bf) {
				logger.Error("HLS", fmt.Sprintf("分片生成失败: id=%d, file=%s: %s", id, file, bf.Cause.Error()))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			logger.Warn("HLS", fmt.Sprintf("等待分片超时: id=%d, file=%s", id, file))
			w.WriteHeader(http.StatusNotFound) // 等待超时视为确实不存在
			return
		}
	}

	contentType := "application/octet-stream"
	cacheControl := ""
	switch {
	case strings.HasSuffix(file, ".m3u8"):
		contentType = "application/vnd.apple.mpegurl"
		cacheControl = "no-store"
	case strings.HasSuffix(file, ".ts"):
		contentType = "video/mp2t"
		cacheControl = "public, max-age=31536000, immutable"
	}
	serveFileWithHeaders(w, p, contentType, cacheControl)
}

func serveFileWithHeaders(w http.ResponseWriter, path, contentType, cacheControl string) {
	f, err := os.Open(path)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// ---------- MV 直传兜底 ----------

func handleStream(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r.PathValue("id"))
	if !ok {
		return
	}
	song, err := getSongByID(id)
	if err != nil || !fileExists(song.Filepath) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	trackParam := r.URL.Query().Get("track")
	hasMultiTrack := song.AudioTracks != nil && *song.AudioTracks >= 2

	// 多音轨 + 指定音轨：ffmpeg 现场重新封装单音轨流（不支持 Range）。
	if trackParam != "" && hasMultiTrack {
		track := int64(0)
		if v, err := strconv.ParseInt(trackParam, 10, 64); err == nil {
			track = v
		}
		maxTrack := *song.AudioTracks - 1
		if track < 0 {
			track = 0
		}
		if track > maxTrack {
			track = maxTrack
		}
		streamSingleTrack(w, r, song.Filepath, track)
		return
	}

	// 单音轨/未指定：源文件直传，支持 Range。
	serveRange(w, r, song.Filepath, "video/mp4")
}

func streamSingleTrack(w http.ResponseWriter, r *http.Request, filepath string, track int64) {
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error",
		"-i", filepath,
		"-map", "0:v:0",
		"-map", fmt.Sprintf("0:a:%d", track),
		"-c", "copy",
		"-movflags", "frag_keyframe+empty_moov+faststart",
		"-f", "mp4",
		"pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		logger.Error("TRANSCODE", "[stream直传兜底] ffmpeg 启动失败: "+err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// 读取 stderr 打日志；客户端断开时终止 ffmpeg。
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		b, _ := io.ReadAll(stderr)
		if msg := strings.TrimSpace(string(b)); msg != "" {
			logger.Warn("TRANSCODE", "[stream直传兜底][ffmpeg] "+msg)
		}
	}()
	go func() {
		select {
		case <-r.Context().Done():
			_ = cmd.Process.Kill()
		case <-stderrDone:
		}
	}()

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "none") // 现场重新封装，长度未知，不支持 Range
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, stdout)
	_ = cmd.Wait()
	<-stderrDone
}

// serveRange 支持 Range 请求的源文件直传（与 Node 版行为一致）。
func serveRange(w http.ResponseWriter, r *http.Request, filepath, contentType string) {
	st, err := os.Stat(filepath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	size := st.Size()
	rangeHeader := r.Header.Get("Range")

	w.Header().Set("Content-Type", contentType)

	if rangeHeader == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		f, err := os.Open(filepath)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		defer f.Close()
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
		return
	}

	start, end, ok := parseRange(rangeHeader, size)
	if !ok {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	f, err := os.Open(filepath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return
	}
	_, _ = io.CopyN(w, f, end-start+1)
}

// parseRange 解析 "bytes=start-end"（不处理后缀形式，与原版一致）。
func parseRange(header string, size int64) (int64, int64, bool) {
	part, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok {
		return 0, 0, false
	}
	se := strings.SplitN(part, "-", 2)
	if len(se) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(se[0]), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if se[1] != "" {
		if v, err := strconv.ParseInt(strings.TrimSpace(se[1]), 10, 64); err == nil && v >= start && v < size {
			end = v
		}
	}
	return start, end, true
}

// parseID 解析路径中的数字 id，非法时返回 400。
func parseID(w http.ResponseWriter, s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return 0, false
	}
	return id, true
}
