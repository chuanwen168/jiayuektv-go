package scanner

import "testing"

type tc struct {
	name                 string
	artist, title, lang, genre string
}

func TestParseFilename(t *testing.T) {
	cases := []tc{
		// 用户报的 bug
		{"卓依婷-花好月圆 - 国语 - 怀旧.mkv", "卓依婷", "花好月圆", "国语", "怀旧"},
		{"胡歌-逍遥叹-国语.mkv", "胡歌", "逍遥叹", "国语", ""},
		// 既有正常格式
		{"郭静 - 心墙.mkv", "郭静", "心墙", "", ""},
		{"郭静 - 心墙-国语-流行歌曲.mkv", "郭静", "心墙", "国语", "流行"},
		{"高进&小沈阳 - 我的好兄弟.mkv", "高进&小沈阳", "我的好兄弟", "", ""},
		{"周杰伦 - 晴天.mp4", "周杰伦", "晴天", "", ""},
		{"胡歌-逍遥叹.mkv", "胡歌", "逍遥叹", "", ""},
		{"郭静-心墙-国语-流行歌曲.mkv", "郭静", "心墙", "国语", "流行"},
		{"歌手-歌名-粤语-摇滚风.mkv", "歌手", "歌名", "粤语", "摇滚"},
		// 歌名含 - 不应误拆
		{"刘德华 - 天若有情-天意.mkv", "刘德华", "天若有情-天意", "", ""},
		// 无歌名仅有歌手
		{"周杰伦.mp4", "周杰伦", "未知歌名", "", ""},
		// 多空格容错
		{"  卓依婷  -  花好月圆  -  国语  -  怀旧  .mkv", "卓依婷", "花好月圆", "国语", "怀旧"},
	}
	for _, c := range cases {
		a, ti, l, g := ParseFilename(c.name)
		if a != c.artist || ti != c.title || l != c.lang || g != c.genre {
			t.Errorf("%q => got (artist=%q title=%q lang=%q genre=%q), want (artist=%q title=%q lang=%q genre=%q)",
				c.name, a, ti, l, g, c.artist, c.title, c.lang, c.genre)
		}
	}
}
