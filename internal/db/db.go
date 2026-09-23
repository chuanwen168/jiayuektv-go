// Package db 负责 SQLite 数据库初始化、建表与兼容性迁移，
// 表结构与原 Node.js 版 db.js 完全一致。
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"ktvhome/internal/logger"
)

// DB 为全局数据库句柄（与原版单例 db 对应）。
var DB *sql.DB

// Song 对应 songs 表的一行。可空字段使用指针以保持 JSON null 语义
// （与 better-sqlite3 返回 null 的行为一致）。
type Song struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	Artist      *string `json:"artist"`
	Filename    string  `json:"filename"`
	Filepath    string  `json:"filepath"`
	Cover       *string `json:"cover"`
	Duration    *int64  `json:"duration"`
	Pinyin      *string `json:"pinyin"`
	PlayCount   int64   `json:"play_count"`
	AudioTracks *int64  `json:"audio_tracks"`
	Language    *string `json:"language"`
	Genre       *string `json:"genre"`
	CreatedAt   string  `json:"created_at"`
}

// QueueEntry 对应点歌队列 JOIN songs 的结果行。
type QueueEntry struct {
	QueueID     int64   `json:"queue_id"`
	Nickname    string  `json:"nickname"`
	IsTop       int64   `json:"is_top"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	SongID      int64   `json:"song_id"`
	Title       string  `json:"title"`
	Artist      *string `json:"artist"`
	Filename    string  `json:"filename"`
	Cover       *string `json:"cover"`
	Duration    *int64  `json:"duration"`
	AudioTracks *int64  `json:"audio_tracks"`
}

// HistoryEntry 对应历史(常唱)榜查询结果。
type HistoryEntry struct {
	Song
	TimesSung int64 `json:"times_sung"`
}

// Init 打开（或创建）数据库并完成建表与迁移。
func Init(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	dbPath := filepath.Join(dataDir, "ktv.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", dbPath)

	var err error
	DB, err = sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}

	if _, err := DB.Exec(schemaSQL); err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}

	// 兼容性迁移：老版本数据库缺少 language/genre 列，幂等补齐。
	if err := migrateColumns(); err != nil {
		logger.Error("DB", "字段迁移失败: "+err.Error())
	} else {
		logger.Info("DB", "数据库初始化完成: "+dbPath)
	}
	return nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS songs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL,
  artist TEXT,
  filename TEXT UNIQUE NOT NULL,
  filepath TEXT NOT NULL,
  cover TEXT,
  duration INTEGER,
  pinyin TEXT,
  play_count INTEGER DEFAULT 0,
  audio_tracks INTEGER,
  language TEXT,
  genre TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 歌手拆分关联表：合唱/多歌手（"歌手1&歌手2"）按 & 拆分后逐位入库，
-- 歌手列表 / 按歌手找歌都从这里查，每位歌手独立可见。
CREATE TABLE IF NOT EXISTS song_artists (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  song_id INTEGER NOT NULL,
  artist TEXT NOT NULL,
  FOREIGN KEY (song_id) REFERENCES songs(id)
);
CREATE INDEX IF NOT EXISTS idx_song_artists_artist ON song_artists(artist);
CREATE INDEX IF NOT EXISTS idx_song_artists_song ON song_artists(song_id);

CREATE TABLE IF NOT EXISTS queue (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  song_id INTEGER NOT NULL,
  nickname TEXT DEFAULT '匿名歌手',
  is_top INTEGER DEFAULT 0,
  status TEXT DEFAULT 'waiting', -- waiting | playing | done
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (song_id) REFERENCES songs(id)
);

CREATE TABLE IF NOT EXISTS history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  song_id INTEGER NOT NULL,
  nickname TEXT,
  played_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS favorites (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  song_id INTEGER NOT NULL,
  device_id TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(song_id, device_id)
);

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT
);
`

// migrateColumns 幂等补齐 songs 表的 audio_tracks/language/genre 列。
func migrateColumns() error {
	rows, err := DB.Query("PRAGMA table_info(songs)")
	if err != nil {
		return err
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	add := func(col, ctype string) error {
		if cols[col] {
			return nil
		}
		_, err := DB.Exec("ALTER TABLE songs ADD COLUMN " + col + " " + ctype)
		return err
	}
	if err := add("audio_tracks", "INTEGER"); err != nil {
		return err
	}
	if err := add("language", "TEXT"); err != nil {
		return err
	}
	return add("genre", "TEXT")
}

// GetSetting 读取 settings 表中的键值。
func GetSetting(key string) (string, bool, error) {
	var v string
	err := DB.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting 写入（或覆盖）settings 表中的键值。
func SetSetting(key, value string) error {
	_, err := DB.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}
