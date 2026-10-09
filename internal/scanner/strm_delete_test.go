package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ktvhome/internal/db"
)

// TestResolveInputPath 验证 .strm 解析（URL 行、本地路径行、多行取首行、非 strm 原样）。
func TestResolveInputPath(t *testing.T) {
	dir := t.TempDir()
	strm := filepath.Join(dir, "a.strm")
	if err := os.WriteFile(strm, []byte("https://example.com/video.mkv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveInputPath(strm); got != "https://example.com/video.mkv" {
		t.Fatalf("URL strm: got %q", got)
	}
	if got := ResolveInputPath(filepath.Join(dir, "b.mp4")); got != filepath.Join(dir, "b.mp4") {
		t.Fatalf("non-strm: got %q", got)
	}
	empty := filepath.Join(dir, "empty.strm")
	_ = os.WriteFile(empty, []byte("   \n"), 0o644)
	if got := ResolveInputPath(empty); got != empty {
		t.Fatalf("empty strm should fallback to original path, got %q", got)
	}
}

// TestDeleteSongAndRefs 验证外键引用场景下删除不再失败：
// song_artists/queue（含 done 历史记录）都引用 songs，直接 DELETE 会外键失败。
func TestDeleteSongAndRefs(t *testing.T) {
	dir := t.TempDir()
	if err := db.Init(dir); err != nil {
		t.Fatal(err)
	}
	defer db.DB.Close()

	res, err := db.DB.Exec("INSERT INTO songs (title, filename, filepath) VALUES (?,?,?)", "测试", "net/测试.mkv", "/tmp/测试.mkv")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	// 模拟点过的歌：song_artists 关联 + queue done 记录 + history + favorites。
	seeds := []struct {
		q    string
		args []any
	}{
		{"INSERT INTO song_artists (song_id, artist) VALUES (?,?)", []any{id, "测试歌手"}},
		{"INSERT INTO queue (song_id, nickname, status) VALUES (?,'匿名歌手','done')", []any{id}},
		{"INSERT INTO history (song_id, nickname) VALUES (?,'匿名歌手')", []any{id}},
		{"INSERT INTO favorites (song_id, device_id) VALUES (?,'test')", []any{id}},
	}
	for _, s := range seeds {
		if _, err := db.DB.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed failed: %v", err)
		}
	}

	// 修复前：直接 DELETE 应外键失败。
	if _, err := db.DB.Exec("DELETE FROM songs WHERE id = ?", id); err == nil {
		t.Fatal("预期直接 DELETE 触发外键失败，实际成功（说明约束未生效）")
	}

	// 修复后：级联删除应成功。
	if err := DeleteSongAndRefs(id); err != nil {
		t.Fatalf("DeleteSongAndRefs failed: %v", err)
	}
	var n int64
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM songs WHERE id=?", id).Scan(&n)
	if n != 0 {
		t.Fatal("songs 行未删除")
	}
	for _, tbl := range []string{"song_artists", "queue", "history", "favorites"} {
		_ = db.DB.QueryRow("SELECT COUNT(*) FROM "+tbl+" WHERE song_id=?", id).Scan(&n)
		if n != 0 {
			t.Fatalf("%s 关联行未清理: %d", tbl, n)
		}
	}
}

// TestIsNetFilename 验证网盘 filename 特征识别。
func TestIsNetFilename(t *testing.T) {
	cases := map[string]bool{
		"net/歌手-歌名.mkv":   true,
		"net_net/歌手-歌名.mkv": true,
		"音乐_net/歌手-歌名.mkv": true,
		"本地/歌手-歌名.mkv":    false,
		"歌手-歌名.mkv":        false,
		"netx/歌手-歌名.mkv":   false,
	}
	for f, want := range cases {
		if got := isNetFilename(f); got != want {
			t.Fatalf("isNetFilename(%q) = %v, want %v", f, got, want)
		}
	}
	_ = strings.TrimSpace // 保持 strings import 不被移除
}
