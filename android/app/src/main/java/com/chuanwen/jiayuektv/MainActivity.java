package com.chuanwen.jiayuektv;

import android.app.AlertDialog;
import android.content.SharedPreferences;
import android.graphics.Bitmap;
import android.graphics.Color;
import android.media.MediaPlayer;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.Spannable;
import android.text.SpannableStringBuilder;
import android.text.style.ForegroundColorSpan;
import android.view.Gravity;
import android.view.KeyEvent;
import android.view.View;
import android.view.WindowManager;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.ImageView;
import android.widget.RadioButton;
import android.widget.RadioGroup;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

import androidx.annotation.Nullable;
import androidx.annotation.OptIn;
import androidx.appcompat.app.AppCompatActivity;
import androidx.appcompat.widget.SwitchCompat;
import androidx.media3.common.C;
import androidx.media3.common.Format;
import androidx.media3.common.MediaItem;
import androidx.media3.common.PlaybackException;
import androidx.media3.common.Player;
import androidx.media3.common.TrackGroup;
import androidx.media3.common.TrackSelectionOverride;
import androidx.media3.common.TrackSelectionParameters;
import androidx.media3.common.Tracks;
import androidx.media3.exoplayer.ExoPlayer;
import androidx.media3.ui.PlayerView;

import com.google.zxing.BarcodeFormat;
import com.google.zxing.common.BitMatrix;
import com.google.zxing.qrcode.QRCodeWriter;

import org.json.JSONObject;

import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/**
 * 家悦K歌 · 安卓 TV 播放端
 * 全屏原生播放（ExoPlayer/HLS），不显示播放列表；
 * 悬浮控制按键无操作自动隐藏；扫码按钮弹出右上角二维码；顶部间歇滚动字幕。
 */
@OptIn(markerClass = androidx.media3.common.util.UnstableApi.class)
public class MainActivity extends AppCompatActivity {

    private static final String PREFS = "jiayue_prefs";
    private static final String KEY_SERVER = "server";
    private static final String KEY_VOICE_DEFAULT = "voice_default";
    private static final long POLL_INTERVAL = 2000;   // 状态轮询
    private static final long HIDE_DELAY = 5000;      // 控制层自动隐藏
    private static final long TICKER_MIN = 10000;     // 字幕最小间隔
    private static final long TICKER_RANGE = 10000;   // 字幕随机区间
    private static final long QR_DURATION = 8000;     // 二维码展示时长

    private ExoPlayer player;
    private PlayerView playerView;
    private FrameLayout tickerHost;
    private TickerView ticker;
    private ImageView qrView;
    private View touchLayer;
    private View controlBar;
    private LinearLayout idleView;
    private TextView idleLogo;
    private TextView tvInfo;
    private TextView effectToast;
    private Button btnPlay, btnRepeat, btnNext, btnVolDown, btnVolUp, btnVoice, btnScan, btnSettings;
    private CtrlWS ctrlWS;

    private final Handler ui = new Handler(Looper.getMainLooper());
    private final ExecutorService exec = Executors.newSingleThreadExecutor();

    private final Runnable pollRunnable = new Runnable() {
        @Override
        public void run() {
            pollState();
            ui.postDelayed(this, POLL_INTERVAL);
        }
    };
    private final Runnable hideRunnable = new Runnable() {
        @Override
        public void run() {
            hideControls();
        }
    };
    private final Runnable tickerRunnable = new Runnable() {
        @Override
        public void run() {
            showNextTicker();
        }
    };
    private final Runnable qrRunnable = new Runnable() {
        @Override
        public void run() {
            qrView.setVisibility(View.GONE);
        }
    };
    private final Runnable effectHideRunnable = new Runnable() {
        @Override
        public void run() {
            if (effectToast != null) effectToast.setVisibility(View.GONE);
        }
    };

    private volatile long playingSongId = -1;
    private String nowPlaying = "";
    private String nowArtist = "";
    private String nextSong = "";
    private String nextArtist = "";
    private String welcome = "家悦K歌，唱响每一刻";
    private int tickerIdx = 0;
    private int tickerStep = 1;   // 1=歌曲播报段 2=欢迎语段（网页版同款两段式）
    private boolean voiceOriginal = true;   // 当前音轨：true=原唱(0) false=伴唱(1)
    private boolean voiceSpecified = false;  // 上一首是否被用户手动指定过音轨（「上次」模式用）
    private int playerErrorCount = 0;

    @Override
    protected void onCreate(@Nullable Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        setContentView(R.layout.activity_main);
        enterImmersive();

        playerView = findViewById(R.id.playerView);
        tickerHost = findViewById(R.id.tickerHost);
        qrView = findViewById(R.id.qrView);
        effectToast = findViewById(R.id.effectToast);
        touchLayer = findViewById(R.id.touchLayer);
        controlBar = findViewById(R.id.controlBar);
        idleView = findViewById(R.id.idleView);
        idleLogo = findViewById(R.id.idleLogo);
        tvInfo = findViewById(R.id.tvInfo);
        btnPlay = findViewById(R.id.btnPlay);
        btnRepeat = findViewById(R.id.btnRepeat);
        btnNext = findViewById(R.id.btnNext);
        btnVolDown = findViewById(R.id.btnVolDown);
        btnVolUp = findViewById(R.id.btnVolUp);
        btnVoice = findViewById(R.id.btnVoice);
        btnScan = findViewById(R.id.btnScan);
        btnSettings = findViewById(R.id.btnSettings);

        // 滚动字幕
        ticker = new TickerView(this);
        tickerHost.addView(ticker, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.WRAP_CONTENT, FrameLayout.LayoutParams.WRAP_CONTENT,
                Gravity.TOP | Gravity.START));
        ticker.setOnFinishedListener(this::onTickerFinished);

        setupPlayer();
        setupControls();

        // 服务器地址
        SharedPreferences sp = getSharedPreferences(PREFS, MODE_PRIVATE);
        String server = sp.getString(KEY_SERVER, "");
        if (server.isEmpty()) {
            showSettingsDialog();
        } else {
            Api.base = server;
            start();
        }
    }

    private void setupPlayer() {
        player = new ExoPlayer.Builder(this).build();
        playerView.setPlayer(player);
        player.setPlayWhenReady(true);
        player.addListener(new Player.Listener() {
            @Override
            public void onPlaybackStateChanged(int state) {
                if (state == Player.STATE_ENDED) {
                    // 播完自动切下一首（服务端按设置决定是否随机续播）
                    postNext();
                }
            }

            @Override
            public void onPlayerError(PlaybackException error) {
                // 播放失败（硬解失败已自动回退软解；仍失败则切歌，避免卡死）
                playerErrorCount++;
                if (playerErrorCount <= 3) {
                    postNext();
                } else {
                    playerErrorCount = 0;
                    tvInfo.setText("播放失败，等待下一首…");
                }
            }
        });
    }

    private void setupControls() {
        btnPlay.setOnClickListener(v -> {
            togglePlayLocal();
            touch();
        });
        btnRepeat.setOnClickListener(v -> {
            repeatLocal();
            touch();
        });
        btnNext.setOnClickListener(v -> {
            postNext();
            touch();
        });
        btnVolDown.setOnClickListener(v -> {
            if (player != null) {
                player.setVolume(Math.max(0f, player.getVolume() - 0.2f));
                tvInfo.setText("音量 " + Math.round(player.getVolume() * 100) + "%");
            }
            touch();
        });
        btnVolUp.setOnClickListener(v -> {
            if (player != null) {
                player.setVolume(Math.min(1f, player.getVolume() + 0.2f));
                tvInfo.setText("音量 " + Math.round(player.getVolume() * 100) + "%");
            }
            touch();
        });
        btnVoice.setOnClickListener(v -> {
            switchVoice();
            touch();
        });
        btnScan.setOnClickListener(v -> {
            showQr();
            touch();
        });
        btnSettings.setOnClickListener(v -> {
            showSettingsDialog();
            touch();
        });

        // 点击屏幕：唤出/隐藏控制层
        touchLayer.setOnClickListener(v -> {
            if (controlBar.getVisibility() == View.VISIBLE) {
                hideControls();
            } else {
                showControls();
            }
        });
    }

    // ---------- 播放与状态 ----------

    /** 播放/暂停切换（本地按钮与控制端共用）。 */
    private void togglePlayLocal() {
        if (player == null) return;
        if (player.getPlayWhenReady()) {
            player.pause();
            btnPlay.setText("播放");
        } else {
            player.play();
            btnPlay.setText("暂停");
        }
    }

    /** 重唱：回到歌曲开头重新播放（本地按钮与控制端共用）。 */
    private void repeatLocal() {
        if (player != null) {
            player.seekTo(0);
            player.play();
            btnPlay.setText("暂停");
        }
    }

    /** 控制端 WebSocket：手机/平板点歌端发来的 control 消息。 */
    private void setupCtrlWS() {
        ctrlWS = new CtrlWS(new CtrlWS.Listener() {
            @Override
            public void onControl(String action, JSONObject msg) {
                handleControl(action, msg);
            }

            @Override
            public void onEffect(String effect, String nickname) {
                handleEffect(effect, nickname);
            }
        });
        ctrlWS.connect();
    }

    /** 氛围互动：播放原生音效 + 顶部浮层（仿网页 TV：#effect-toast）。 */
    private void handleEffect(String effect, String nickname) {
        int resId;
        String label;
        switch (effect) {
            case "cheer":    resId = R.raw.cheer;    label = "欢呼"; break;
            case "applause": resId = R.raw.applause; label = "鼓掌"; break;
            case "scream":   resId = R.raw.scream;   label = "尖叫"; break;
            case "whistle":  resId = R.raw.whistle;  label = "口哨"; break;
            case "boo":      resId = R.raw.boo;      label = "喝倒彩"; break;
            case "encore":   resId = R.raw.encore;   label = "再来一首"; break;
            default: return;
        }
        String who = (nickname == null || nickname.isEmpty()) ? "匿名歌手" : nickname;
        showEffectToast("🎉 " + who + " · " + label);
        playEffectSfx(resId, player != null ? player.getVolume() : 1f);
    }

    /** 浮层：顶部居中弹出，2.6 秒后消失（与网页版一致）。 */
    private void showEffectToast(String text) {
        if (effectToast == null) return;
        effectToast.setText(text);
        effectToast.setVisibility(View.VISIBLE);
        effectToast.setAlpha(0f);
        effectToast.animate().alpha(1f).setDuration(220).start();
        ui.removeCallbacks(effectHideRunnable);
        ui.postDelayed(effectHideRunnable, 2600);
    }

    /** 播放原生音效（MediaPlayer，播放完自动释放；音量跟随播放器音量）。 */
    private void playEffectSfx(int resId, float vol) {
        try {
            MediaPlayer mp = MediaPlayer.create(this, resId);
            if (mp == null) return;
            mp.setVolume(Math.max(0f, Math.min(1f, vol)), Math.max(0f, Math.min(1f, vol)));
            mp.setOnCompletionListener(m -> m.release());
            mp.setOnErrorListener((m, what, extra) -> { m.release(); return true; });
            mp.start();
        } catch (Exception ignored) {
        }
    }

    private void handleControl(String action, JSONObject msg) {
        switch (action) {
            case "play_pause":
                togglePlayLocal();
                break;
            case "voice":
                switchVoice();
                break;
            case "repeat":
                repeatLocal();
                break;
            case "next":
                postNext();
                break;
            case "volume_up":
                if (player != null) player.setVolume(Math.min(1f, player.getVolume() + 0.1f));
                break;
            case "volume_down":
                if (player != null) player.setVolume(Math.max(0f, player.getVolume() - 0.1f));
                break;
            default:
                // eq/fullscreen/home/focus_* 等已随手机端遥控入口下线的动作忽略
                break;
        }
    }

    private void start() {
        ui.removeCallbacks(pollRunnable);
        ui.removeCallbacks(tickerRunnable);
        ui.post(pollRunnable);
        scheduleTicker();
        if (ctrlWS == null) setupCtrlWS();
        else ctrlWS.connect();
        exec.execute(this::loadWelcome);
    }

    private void pollState() {
        if (Api.base.isEmpty()) return;
        exec.execute(() -> {
            try {
                Api.HttpResult r = Api.get("/api/player/state");
                if (r.code != 200) {
                    ui.post(() -> tvInfo.setText("未连接服务器 (" + Api.base + ")"));
                    return;
                }
                JSONObject data = new JSONObject(r.body);
                JSONObject playing = data.optJSONObject("playing");
                long sid = -1;
                String title = "", artist = "", ntitle = "", nartist = "";
                if (playing != null) {
                    sid = playing.optLong("song_id");
                    title = playing.optString("title", "");
                    artist = playing.optString("artist", "");
                }
                JSONObject next = data.optJSONObject("next");
                if (next != null) {
                    ntitle = next.optString("title", "");
                    nartist = next.optString("artist", "");
                }
                final long fSid = sid;
                final String fTitle = title, fArtist = artist, fNTitle = ntitle, fNArtist = nartist;
                ui.post(() -> onState(fSid, fTitle, fArtist, fNTitle, fNArtist));
            } catch (Exception e) {
                ui.post(() -> tvInfo.setText("服务器连接中…"));
            }
        });
    }

    private void onState(long sid, String title, String artist, String ntitle, String nartist) {
        nowPlaying = title;
        nowArtist = artist;
        nextSong = ntitle;
        nextArtist = nartist;
        if (sid <= 0) {
            // 无播放歌曲（未点歌 / 切完最后一首且不随机）：显示待机大屏并停止播放
            showIdle();
            tvInfo.setText("请点歌");
            playingSongId = -1;
            btnPlay.setText("暂停");
            if (player != null) {
                player.stop();
                player.clearMediaItems();
            }
            return;
        }
        hideIdle();
        tvInfo.setText("正在播放：" + (title.isEmpty() ? "请点歌" : title + " - " + artist));
        if (sid != playingSongId) {
            playingSongId = sid;
            if (player != null) {
                playerErrorCount = 0;
                btnPlay.setText("暂停");
                // 从 0 位置起播（AV1/长关键帧间隔源兜底，防止跳到中间）。
                // 渐进式转码的 event playlist 在转码完成前没有 ENDLIST，ExoPlayer
                // 会把它当 live 流从"live edge（末尾）"起播 → 表现为首次播放从中间开始。
                // 设置超大的 targetOffsetMs（24h）强制从最老分片（=歌曲开头）起播，
                // 转码完成（有 ENDLIST）后自动回到 VOD 正常从头播放。
                MediaItem item = new MediaItem.Builder()
                        .setUri(Api.base + "/hls/" + sid + "/master.m3u8")
                        .setLiveConfiguration(new MediaItem.LiveConfiguration.Builder()
                                .setTargetOffsetMs(24L * 60 * 60 * 1000)
                                .build())
                        .build();
                player.setMediaItem(item, 0);
                player.prepare();
                player.play();
                // 按「切歌默认请求」设置应用音轨默认（伴唱/原唱/上次）
                applyVoiceDefault();
            }
        }
    }

    /** 显示待机大屏（仿网页 TV：家悦K歌 logo + 播放端已就绪）。 */
    private void showIdle() {
        if (idleView != null && idleView.getVisibility() != View.VISIBLE) {
            applyLogoGradient();
            idleView.setVisibility(View.VISIBLE);
        }
    }

    private void hideIdle() {
        if (idleView != null && idleView.getVisibility() == View.VISIBLE) {
            idleView.setVisibility(View.GONE);
        }
    }

    /** logo 渐变色（网页版：36d9f7→c736f7→ff4f9b）。 */
    private void applyLogoGradient() {
        if (idleLogo == null || idleLogo.getWidth() <= 0) return;
        android.graphics.LinearGradient g = new android.graphics.LinearGradient(
                0, 0, idleLogo.getWidth(), 0,
                new int[]{0xFF36D9F7, 0xFFC736F7, 0xFFFF4F9B},
                null, android.graphics.Shader.TileMode.CLAMP);
        idleLogo.getPaint().setShader(g);
        idleLogo.invalidate();
    }

    /** 切下一首：通知服务端后延迟轮询。 */
    private void postNext() {
        exec.execute(() -> {
            try {
                Api.post("/api/queue/next", new JSONObject());
            } catch (Exception ignored) {
            }
        });
        ui.postDelayed(() -> {
            playingSongId = -1;
            pollState();
        }, 600);
    }

    /** 原唱/伴唱：HLS 多音轨切换。 */
    private void switchVoice() {
        voiceSpecified = true; // 用户手动指定过音轨，「上次」模式据此沿用
        int target = voiceOriginal ? 1 : 0;
        voiceOriginal = !voiceOriginal;
        btnVoice.setText(voiceOriginal ? "伴唱" : "原唱");
        switchVoiceTrack(target);
        long sid = playingSongId;
        exec.execute(() -> {
            try {
                JSONObject body = new JSONObject();
                body.put("song_id", sid);
                body.put("mode", "tracks");
                body.put("to", target == 1 ? "accompaniment" : "original");
                Api.post("/api/voice/switch", body);
            } catch (Exception ignored) {
            }
        });
    }

    /** 切歌默认请求：新歌加载后按设置应用音轨。
     *  伴唱(accompaniment)：一律切伴唱；原唱(original)：一律原唱；
     *  上次(last)：沿用上一首的音轨，上一首没被手动指定过则默认伴奏。 */
    private void applyVoiceDefault() {
        String def = getSharedPreferences(PREFS, MODE_PRIVATE).getString(KEY_VOICE_DEFAULT, "last");
        if ("original".equals(def)) {
            voiceOriginal = true;
            btnVoice.setText("伴唱");
            switchVoiceTrack(0);
        } else if ("accompaniment".equals(def)) {
            voiceOriginal = false;
            btnVoice.setText("原唱");
            switchVoiceTrack(1);
        } else { // last
            if (voiceSpecified) {
                // 上一首被指定过：沿用其音轨
                if (!voiceOriginal) switchVoiceTrack(1);
            } else {
                // 上一首没指定：默认伴奏
                voiceOriginal = false;
                btnVoice.setText("原唱");
                switchVoiceTrack(1);
            }
        }
    }

    private void switchVoiceTrack(int trackIndex) {
        switchVoiceTrackAttempt(trackIndex, 0);
    }

    /** 带重试的音轨切换：Tracks 未就绪时 300ms 后重试，最多 5 次。 */
    private void switchVoiceTrackAttempt(final int trackIndex, final int attempt) {
        if (player == null) return;
        int state = tryApplyVoiceTrack(player, trackIndex);
        if (state == 1) {
            return; // 已切换
        }
        if (state == 0) {
            tvInfo.setText((trackIndex == 1 ? "未找到伴唱音轨" : "未找到原唱音轨") + "（" + voiceDiag + "）");
            return;
        }
        if (attempt < 5) {
            ui.postDelayed(() -> switchVoiceTrackAttempt(trackIndex, attempt + 1), 300);
        } else {
            tvInfo.setText("音轨切换失败，请稍后重试");
        }
    }

    /** 诊断：音频组结构（组数/每组音轨数/label）。 */
    private String voiceDiag = "";

    /**
     * 尝试切换音轨。兼容两种 HLS 结构：
     *   a) 单音频组内含全部音轨（length>1，override 组内索引）；
     *   b) 每个音轨各自成组（length=1，按 label/format 识别原唱/伴唱组后 override）。
     * 返回 1=已切换 0=有音频组但无匹配音轨 -1=Tracks 未就绪。
     */
    private int tryApplyVoiceTrack(ExoPlayer p, int trackIndex) {
        Tracks tracks = p.getCurrentTracks();
        if (tracks == null) return -1;
        int audioGroups = 0;
        boolean applied = false;
        StringBuilder diag = new StringBuilder();
        for (Tracks.Group g : tracks.getGroups()) {
            if (g.getType() != C.TRACK_TYPE_AUDIO) continue;
            audioGroups++;
            TrackGroup group = g.getMediaTrackGroup();
            Format f = group.getFormat(0);
            diag.append("组").append(audioGroups).append("=").append(group.length);
            if (f.label != null && !f.label.isEmpty()) diag.append("/").append(f.label);
            diag.append(";");
            if (group.length > 1) {
                // 结构 a：同组多音轨，override 组内索引
                p.setTrackSelectionParameters(p.getTrackSelectionParameters().buildUpon()
                        .setOverrideForType(new TrackSelectionOverride(group, trackIndex))
                        .build());
                applied = true;
                continue;
            }
            // 结构 b：单条音轨组，按 label/format 识别
            String label = f.label == null ? "" : f.label;
            String fid = f.id == null ? "" : f.id;
            boolean isAcc = label.contains("伴唱") || fid.contains("audio1") || fid.endsWith("/1");
            boolean isOrig = label.contains("原唱") || fid.contains("audio0") || fid.endsWith("/0");
            if (trackIndex == 1 && isAcc) {
                p.setTrackSelectionParameters(p.getTrackSelectionParameters().buildUpon()
                        .setOverrideForType(new TrackSelectionOverride(group, 0))
                        .build());
                applied = true;
            } else if (trackIndex == 0 && isOrig) {
                // 之前可能已 override 到伴唱组，切回原唱必须重新 override 原唱组
                p.setTrackSelectionParameters(p.getTrackSelectionParameters().buildUpon()
                        .setOverrideForType(new TrackSelectionOverride(group, 0))
                        .build());
                applied = true;
            }
        }
        voiceDiag = diag.toString();
        if (applied) return 1;
        return audioGroups > 0 ? 0 : -1;
    }

    // ---------- 悬浮控制层 ----------

    private void showControls() {
        controlBar.setVisibility(View.VISIBLE);
        controlBar.animate().alpha(1f).setDuration(200).start();
        btnPlay.requestFocus();
        resetHideTimer();
    }

    private void hideControls() {
        controlBar.animate().alpha(0f).setDuration(300).withEndAction(() -> {
            controlBar.setVisibility(View.INVISIBLE);
            controlBar.setAlpha(1f);
        }).start();
    }

    private void resetHideTimer() {
        ui.removeCallbacks(hideRunnable);
        ui.postDelayed(hideRunnable, HIDE_DELAY);
    }

    /** 任意操作后调用：重置自动隐藏计时。 */
    private void touch() {
        if (controlBar.getVisibility() != View.VISIBLE) showControls();
        else resetHideTimer();
    }

    @Override
    public boolean dispatchKeyEvent(KeyEvent event) {
        if (event.getAction() == KeyEvent.ACTION_DOWN) {
            switch (event.getKeyCode()) {
                case KeyEvent.KEYCODE_BACK:
                    showExitDialog();
                    return true;
                case KeyEvent.KEYCODE_MENU:
                    showSettingsDialog();
                    return true;
                case KeyEvent.KEYCODE_DPAD_UP:
                case KeyEvent.KEYCODE_DPAD_DOWN:
                case KeyEvent.KEYCODE_DPAD_LEFT:
                case KeyEvent.KEYCODE_DPAD_RIGHT:
                    // 控制层隐藏时：方向键先唤出控制层（之后按键正常移动焦点）
                    if (controlBar.getVisibility() != View.VISIBLE) {
                        showControls();
                        return true;
                    }
                    break;
                case KeyEvent.KEYCODE_DPAD_CENTER:
                case KeyEvent.KEYCODE_ENTER:
                    // 确认键：交给当前聚焦控件处理
                    break;
                default:
                    break;
            }
        }
        return super.dispatchKeyEvent(event);
    }

    /** 返回键二次确认退出。 */
    private void showExitDialog() {
        new AlertDialog.Builder(this)
                .setTitle("退出")
                .setMessage("确定退出家悦K歌吗？")
                .setNegativeButton("取消", null)
                .setPositiveButton("退出", (d, w) -> finish())
                .show();
    }

    // ---------- 扫码 ----------

    /** 扫码：显示则隐藏，隐藏则显示（右上角二维码）。 */
    private void showQr() {
        if (qrView.getVisibility() == View.VISIBLE) {
            qrView.setVisibility(View.GONE);
            ui.removeCallbacks(qrRunnable);
            return;
        }
        String url = Api.base + "/m/";
        try {
            QRCodeWriter w = new QRCodeWriter();
            BitMatrix m = w.encode(url, BarcodeFormat.QR_CODE, 400, 400);
            Bitmap bmp = Bitmap.createBitmap(m.getWidth(), m.getHeight(), Bitmap.Config.ARGB_8888);
            for (int x = 0; x < m.getWidth(); x++) {
                for (int y = 0; y < m.getHeight(); y++) {
                    bmp.setPixel(x, y, m.get(x, y) ? Color.BLACK : Color.WHITE);
                }
            }
            qrView.setImageBitmap(bmp);
            qrView.setVisibility(View.VISIBLE);
            ui.removeCallbacks(qrRunnable);
            ui.postDelayed(qrRunnable, QR_DURATION);
        } catch (Exception e) {
            Toast.makeText(this, "二维码生成失败", Toast.LENGTH_SHORT).show();
        }
    }

    // ---------- 滚动字幕 ----------

    private void scheduleTicker() {
        ui.removeCallbacks(tickerRunnable);
        ui.postDelayed(tickerRunnable, TICKER_MIN + (long) (Math.random() * TICKER_RANGE));
    }

    private void showNextTicker() {
        tickerStep = 1;
        ticker.startTicker(buildSongTicker());
    }

    /** 歌曲播报段（网页版同款）：▶ 正在播放(金色) │ ⏭ 下一曲(绿色)。 */
    private CharSequence buildSongTicker() {
        SpannableStringBuilder sb = new SpannableStringBuilder();
        int gold = Color.rgb(255, 224, 138);    // tk-playing
        int green = Color.rgb(179, 255, 214);   // tk-next
        int dim = Color.argb(102, 255, 255, 255); // │ 分隔符低透

        String play = "▶ 正在播放：" + (nowPlaying.isEmpty() ? "请点歌" : nowPlaying + "　" + nowArtist);
        int s0 = sb.length();
        sb.append(play);
        sb.setSpan(new ForegroundColorSpan(gold), s0, s0 + play.length(), Spannable.SPAN_EXCLUSIVE_EXCLUSIVE);

        String sep = " │ ";
        int s1 = sb.length();
        sb.append(sep);
        sb.setSpan(new ForegroundColorSpan(dim), s1, s1 + sep.length(), Spannable.SPAN_EXCLUSIVE_EXCLUSIVE);

        String next = "⏭ 下一曲：" + (nextSong.isEmpty() ? "请点歌" : nextSong + "　" + nextArtist);
        int s2 = sb.length();
        sb.append(next);
        sb.setSpan(new ForegroundColorSpan(green), s2, s2 + next.length(), Spannable.SPAN_EXCLUSIVE_EXCLUSIVE);
        return sb;
    }

    /** 字幕跑完一段后：先接欢迎语段，全部播完再等 10~20 秒随机间隔出现。 */
    private void onTickerFinished() {
        if (tickerStep == 1) {
            tickerStep = 2;
            SpannableStringBuilder sb = new SpannableStringBuilder(welcome);
            sb.setSpan(new ForegroundColorSpan(Color.rgb(255, 236, 180)),
                    0, sb.length(), Spannable.SPAN_EXCLUSIVE_EXCLUSIVE);
            ticker.startTicker(sb);
        } else {
            scheduleTicker();
        }
    }

    private void loadWelcome() {
        try {
            Api.HttpResult r = Api.get("/api/welcome");
            if (r.code == 200) {
                String w = new JSONObject(r.body).optString("welcome", "");
                if (!w.isEmpty()) welcome = w;
            }
        } catch (Exception ignored) {
        }
    }

    // ---------- 设置（服务器地址 / 播完随机播放） ----------

    /** 设置页：改 IP + 播完随机播放开关（默认关闭，手动开启）。 */
    private void showSettingsDialog() {
        SharedPreferences sp = getSharedPreferences(PREFS, MODE_PRIVATE);
        String cur = sp.getString(KEY_SERVER, "");

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        int pad = dp(24);
        root.setPadding(pad, pad / 2, pad, 0);

        TextView t1 = new TextView(this);
        t1.setText("服务器地址（IP:端口）");
        EditText et = new EditText(this);
        et.setHint("例如 192.168.1.100:8086");
        et.setText(cur.replace("http://", ""));
        root.addView(t1);
        root.addView(et);

        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setClickable(true);
        row.setFocusable(true);
        row.setPadding(0, dp(6), 0, dp(6));
        TextView t2 = new TextView(this);
        t2.setText("播完随机播放");
        LinearLayout.LayoutParams lp2 = new LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f);
        t2.setLayoutParams(lp2);
        SwitchCompat sw = new SwitchCompat(this);
        sw.setClickable(true);
        sw.setFocusable(true);
        row.addView(t2);
        row.addView(sw);
        root.addView(row);
        // 整行可点：点文字也能开关（Switch 本体点击区域太小，遥控器/手指容易点空）
        row.setOnClickListener(v -> sw.toggle());

        // 切歌默认请求：伴唱 / 原唱 / 上次（沿用上一首，上一首没指定默认伴奏）
        TextView t3 = new TextView(this);
        t3.setText("切歌默认请求");
        t3.setPadding(0, dp(10), 0, 0);
        root.addView(t3);
        RadioGroup rg = new RadioGroup(this);
        rg.setOrientation(RadioGroup.HORIZONTAL);
        RadioButton rbAcc = new RadioButton(this);
        rbAcc.setText("伴唱");
        RadioButton rbOrig = new RadioButton(this);
        rbOrig.setText("原唱");
        RadioButton rbLast = new RadioButton(this);
        rbLast.setText("上次");
        rbAcc.setPadding(dp(8), 0, dp(8), 0);
        rbOrig.setPadding(dp(8), 0, dp(8), 0);
        rbLast.setPadding(dp(8), 0, 0, 0);
        rg.addView(rbAcc);
        rg.addView(rbOrig);
        rg.addView(rbLast);
        root.addView(rg);
        String vdef = sp.getString(KEY_VOICE_DEFAULT, "last");
        if ("original".equals(vdef)) rbOrig.setChecked(true);
        else if ("accompaniment".equals(vdef)) rbAcc.setChecked(true);
        else rbLast.setChecked(true);

        // 载入当前随机开关状态。用 userTouched 防止异步响应回来覆盖用户刚做的操作：
        // 网络慢时用户先点了开关，加载结果稍后才到，不能把它再拉回旧值。
        final boolean[] userTouched = {false};
        sw.setOnCheckedChangeListener((b, checked) -> userTouched[0] = true);
        exec.execute(() -> {
            try {
                Api.HttpResult r = Api.get("/api/player/settings");
                if (r.code == 200) {
                    boolean v = new JSONObject(r.body).optBoolean("autoplay_random", false);
                    ui.post(() -> {
                        if (!userTouched[0]) sw.setChecked(v);
                    });
                }
            } catch (Exception ignored) {
            }
        });

        new AlertDialog.Builder(this)
                .setTitle("设置")
                .setView(root)
                .setNegativeButton("取消", null)
                .setPositiveButton("保存", (d, w) -> {
                    String v = et.getText().toString().trim().replace("http://", "");
                    boolean random = sw.isChecked();
                    int rid = rg.getCheckedRadioButtonId();
                    String vd = rid == rbAcc.getId() ? "accompaniment"
                            : rid == rbOrig.getId() ? "original" : "last";
                    sp.edit().putString(KEY_VOICE_DEFAULT, vd).apply();
                    if (!v.isEmpty()) {
                        String url = v.startsWith("http") ? v : "http://" + v;
                        sp.edit().putString(KEY_SERVER, url).apply();
                        Api.base = url;
                        restartPlayback();
                        Toast.makeText(this, "已保存，连接 " + url, Toast.LENGTH_SHORT).show();
                    }
                    exec.execute(() -> {
                        try {
                            JSONObject b = new JSONObject();
                            b.put("autoplay_random", random);
                            Api.put("/api/player/settings", b);
                        } catch (Exception ignored) {
                        }
                    });
                })
                .setCancelable(true)
                .show();
    }

    private void restartPlayback() {
        playingSongId = -1;
        if (player != null) {
            player.stop();
            player.clearMediaItems();
        }
        start();
    }
    private int dp(int v) {
        return Math.round(getResources().getDisplayMetrics().density * v);
    }

    // ---------- 生命周期 ----------

    private void enterImmersive() {
        getWindow().getDecorView().setSystemUiVisibility(
                View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
                        | View.SYSTEM_UI_FLAG_FULLSCREEN
                        | View.SYSTEM_UI_FLAG_HIDE_NAVIGATION
                        | View.SYSTEM_UI_FLAG_LAYOUT_STABLE);
    }

    @Override
    protected void onResume() {
        super.onResume();
        if (player != null && player.getPlayWhenReady()) player.play();
    }

    @Override
    protected void onPause() {
        super.onPause();
        if (player != null && player.getPlayWhenReady()) player.pause();
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        ui.removeCallbacksAndMessages(null);
        if (ctrlWS != null) ctrlWS.close();
        if (player != null) {
            player.release();
            player = null;
        }
        exec.shutdownNow();
    }
}
