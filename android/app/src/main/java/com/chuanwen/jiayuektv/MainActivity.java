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
import android.util.Log;
import android.view.Gravity;
import android.view.KeyEvent;
import android.view.View;
import android.view.WindowManager;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.RadioButton;
import android.widget.RadioGroup;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import androidx.annotation.Nullable;
import androidx.appcompat.app.AppCompatActivity;
import androidx.appcompat.widget.SwitchCompat;
import androidx.media3.ui.AspectRatioFrameLayout;

import com.google.zxing.BarcodeFormat;
import com.google.zxing.common.BitMatrix;
import com.google.zxing.qrcode.QRCodeWriter;

import org.json.JSONArray;
import org.json.JSONObject;

import java.net.HttpURLConnection;
import java.net.InetAddress;
import java.net.NetworkInterface;
import java.net.URL;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Enumeration;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;

/**
 * 家悦K歌 · 安卓 TV 播放端
 *
 * 播放策略（设置页规则）：
 *   - 默认 HLS 模式，固定使用 Media3（服务端已转码，起播快、切歌不卡顿）；
 *   - 直连模式默认使用 VLC（全格式直解），可手动切换 Media3；
 *   - 直连播放失败 / 不支持 / 10s 未起播 → 自动回退 HLS(Media3)；
 *   - 原唱/伴唱切换：HLS 走 TrackSelector 原位无缝切换；WS 广播指令只执行
 *     切换、不回发上报，避免与服务端广播形成"疯狂切伴奏"死循环。
 * 其余：播放超过 30s 自动预热下一首；悬浮控制无操作自动隐藏；扫码点歌；
 * 顶部滚动字幕；氛围音效。
 */
public class MainActivity extends AppCompatActivity {

    private static final String PREFS = "jiayue_prefs";
    private static final String KEY_SERVER = "server";
    private static final String KEY_VOICE_DEFAULT = "voice_default";
    private static final String KEY_PLAY_MODE = "play_mode";   // direct | hls（默认 hls）
    private static final String KEY_CORE = "core";             // media3 | vlc（直连默认 vlc）
    private static final String KEY_FILL_SCREEN = "fill_screen"; // 播放画面撑满屏幕缩放开关
    private static final String KEY_RANDOM_IDLE_MIN = "random_idle_min"; // 随机空闲自动停止（分钟，0=关闭，默认30）
    private static final int RANDOM_IDLE_DEFAULT_MIN = 30;
    private static final String KEY_CFG_VER = "cfg_ver";       // 设置结构版本（升级迁移用）
    private static final int CFG_VER_CURRENT = 3;              // v3：新增撑满屏幕开关
    private static final String TAG = "JYKTV";             // 诊断日志 tag（临时定位用）
    private static final long POLL_INTERVAL = 2000;   // 状态轮询
    private static final long HIDE_DELAY = 5000;      // 控制层自动隐藏
    private static final long TICKER_MIN = 6000;      // 字幕最小间隔
    private static final long TICKER_RANGE = 6000;    // 字幕随机区间
    private static final long QR_DURATION = 8000;     // 二维码展示时长
    private static final long PRELOAD_POSITION_MS = 30000; // 播放超过 30s 预热下一首
    private static final long DIRECT_WATCHDOG_MS = 10000;  // 直连起播看门狗：10s 未起播视为不支持
    private static final long HLS_WATCHDOG_MS = 15000;      // HLS 起播看门狗：15s 未真正起播（卡 BUFFERING）视为加载异常
    private static final int HLS_WATCHDOG_MAX_TRIES = 2;    // HLS 看门狗最大重载次数，超限切下一首

    private PlayerCore core;
    private String coreKind = "media3";   // 当前活动内核
    private boolean hlsFallback = false;  // 已回退到 HLS（直连失败/不支持）
    private float volume = 1f;
    private volatile boolean userPaused = false;
    private long lastToggleAt = 0; // togglePlayLocal 防抖时间戳（控制端双发/连点时 300ms 内只响应一次）
    private long lastVoiceAt = 0;  // switchVoice 远程广播防抖时间戳（多端重复广播 500ms 内只响应一次）
    private float[] lastEqGains;   // 最近一次控制端下发的均衡器增益（切歌/切内核后重应用）

    private androidx.media3.ui.PlayerView playerView;
    private org.videolan.libvlc.util.VLCVideoLayout vlcView;
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

    // 随机播放临时开关（手机端点歌台 WS 消息置位；仅本次运行生效，不保存设置）
    private Boolean suiji = false;

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
    /** 直连起播看门狗：直连模式下 10s 未真正起播（解码不支持/卡死）→ 自动回退 HLS。
     * 仅对"从未进入过播放状态"的起播失败生效；已正常播放过（含暂停恢复）不回退，
     * 避免暂停后恢复失败被误判为"直连不支持"而跳 HLS。 */
    private final Runnable directWatchdogRunnable = new Runnable() {
        @Override
        public void run() {
            if (!hlsFallback && "direct".equals(playMode())
                    && core != null && !core.isPlaying() && !userPaused && playingSongId > 0
                    && !directEverPlayed) {
                fallbackToHls("直连播放无响应（可能不支持），已自动切换 HLS");
            }
        }
    };

    /** HLS 起播看门狗：Media3 卡 BUFFERING 时不会触发 onError（缓冲不是错误事件），
     * 表现为一直转圈。起播后 15s 未真正进入播放 → 重载当前 HLS 流（转码完成/已缓存则秒开）；
     * 重载超限仍无响应 → 切下一首，避免永久转圈。仅对"从未进入过播放状态"的起播生效。 */
    private final Runnable hlsWatchdogRunnable = new Runnable() {
        @Override
        public void run() {
            if (core == null || playingSongId <= 0 || userPaused || hlsEverPlayed) return;
            if (!"hls".equals(playMode()) && !hlsFallback) return; // 仅 HLS 场景
            Log.d(TAG, "HLS_WATCHDOG: tries=" + hlsWatchdogTries + " playing=" + core.isPlaying() + " userPaused=" + userPaused);
            hlsWatchdogTries++;
            if (hlsWatchdogTries > HLS_WATCHDOG_MAX_TRIES) {
                hlsWatchdogTries = 0;
                tvInfo.setText("播放无响应，已切下一首");
                postNext();
                return;
            }
            tvInfo.setText("加载较慢，正在重试…");
            ensureCore("media3");
            applyVoiceDefault();
            core.play(hlsUrl(playingSongId));
            startHlsWatchdog();
        }
    };

    private volatile long playingSongId = -1;
    // 直连模式本首歌是否进入过真正播放（watchdog 据此避免暂停恢复失败被误判回退 HLS）
    private volatile boolean directEverPlayed = false;
    // HLS 模式本首歌是否进入过真正播放（HLS 看门狗据此避免已播后误重载）
    private volatile boolean hlsEverPlayed = false;
    // HLS 看门狗本首歌已重载次数（超过上限切下一首）
    private volatile int hlsWatchdogTries = 0;
    // 直连当前流相对歌曲的起始偏移（ms）：start 续播后播放器位置从 0 重计，
    // 歌曲真实位置 = currentStreamOffsetMs + core.getPosition()，切音轨时累计。
    private volatile long currentStreamOffsetMs = 0;
    private String nowPlaying = "";
    private String nowArtist = "";
    private String nextSong = "";
    private String nextArtist = "";
    private long nextSongId = -1;
    private long lastPreloadSongId = -1;   // 已预热过的下一曲 id
    private String welcome = "家悦K歌，唱响每一刻";
    private int tickerIdx = 0;
    private int tickerStep = 1;   // 1=歌曲播报段 2=欢迎语段
    private boolean voiceOriginal = true;   // 当前音轨：true=原唱(0) false=伴唱(1)
    private boolean voiceSpecified = false;  // 上一首是否被用户手动指定过音轨（「上次」模式用）
    private volatile int playerErrorCount = 0;
    // 内核代数：ensureCore 每次切换 +1。旧内核的异步事件回调（可能迟到）据此丢弃
    private volatile int coreEpoch = 0;

    private final PlayerCore.Listener coreListener = new PlayerCore.Listener() {
        @Override
        public void onEnded() {
            // 播完自动切下一首（服务端按设置决定是否随机续播）。
            // 延迟执行 + 校验内核代数：切换内核后到达的旧事件一律丢弃
            final int epoch = coreEpoch;
            ui.post(() -> {
                if (epoch == coreEpoch) postNext();
            });
        }

        @Override
        public void onError(String message) {
            // 直连失败/不支持 → 自动回退 HLS(Media3)；HLS 再失败 → 切歌，避免卡死。
            // 延迟执行 + 校验内核代数：旧内核 release 时的错误事件不影响新内核
            final int epoch = coreEpoch;
            ui.post(() -> {
                if (epoch != coreEpoch) return;
                if (!hlsFallback && "direct".equals(playMode())) {
                    fallbackToHls("直连播放失败，已自动切换 HLS");
                    return;
                }
                playerErrorCount++;
                if (playerErrorCount <= 3) {
                    postNext();
                } else {
                    playerErrorCount = 0;
                    tvInfo.setText("播放失败，等待下一首…");
                }
            });
        }
    };

    @Override
    protected void onCreate(@Nullable Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        setContentView(R.layout.activity_main);
        enterImmersive();

        playerView = findViewById(R.id.playerView);
        vlcView = findViewById(R.id.vlcView);
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

        // 升级迁移：v2 起默认 HLS 模式 + 直连默认 VLC（老版本默认 direct/media3 会被纠正）
        migrateSettings();

        // 先建好默认内核并挂到显示容器
        SharedPreferences sp = getSharedPreferences(PREFS, MODE_PRIVATE);
        ensureCore("hls".equals(sp.getString(KEY_PLAY_MODE, "hls"))
                ? "media3" : sp.getString(KEY_CORE, "vlc"));
        core.setVolume(volume);
        setupControls();

        // 服务器地址
        String server = sp.getString(KEY_SERVER, "");
        if (server.isEmpty()) {
            showSettingsDialog();
        } else {
            Api.base = server;
            start();
        }
    }

    /** v2 设置迁移：老安装（默认 direct/media3）改为默认 HLS + 直连默认 VLC。 */
    private void migrateSettings() {
        SharedPreferences sp = getSharedPreferences(PREFS, MODE_PRIVATE);
        int ver = sp.getInt(KEY_CFG_VER, 0);
        if (ver >= CFG_VER_CURRENT) return;
        sp.edit()
                .putString(KEY_PLAY_MODE, "hls")
                .putString(KEY_CORE, "vlc")
                .putInt(KEY_CFG_VER, CFG_VER_CURRENT)
                .apply();
    }

    // ---------- 播放内核 ----------

    private String playMode() {
        return getSharedPreferences(PREFS, MODE_PRIVATE).getString(KEY_PLAY_MODE, "hls");
    }

    /** 直连内核（默认 vlc）。HLS 模式固定 media3，不读此值。 */
    private String directCore() {
        return getSharedPreferences(PREFS, MODE_PRIVATE).getString(KEY_CORE, "vlc");
    }

    private PlayerCore createCore(String kind) {
        if ("vlc".equals(kind)) {
            try {
                return new VLCCore(this);
            } catch (Throwable t) {
                // VLC 库加载失败（so 缺失/JNI 不支持等）→ 回退 Media3，避免闪退
                return new Media3Core(this);
            }
        }
        return new Media3Core(this);
    }

    /** 确保当前内核为指定类型（切换时先解绑监听再释放旧的，并重新挂载显示容器）。 */
    private void ensureCore(String kind) {
        if (core != null && coreKind.equals(kind)) return;
        if (core != null) {
            // 先解绑监听再 release：旧内核 release 时触发的异步事件（EndReached/EncounteredError）
            // 不再回调 coreListener，避免干扰新内核的播放流程（点歌后被旧内核事件切歌/回退）
            core.setListener(null);
            core.release();
            core = null;
        }
        coreEpoch++; // 旧内核的异步回调（含已排队 Runnable）全部失效
        coreKind = kind;
        core = createCore(kind);
        core.setVolume(volume);
        core.setListener(coreListener);
        if ("vlc".equals(kind)) {
            vlcView.setVisibility(View.VISIBLE);
            playerView.setVisibility(View.GONE);
            core.attach(vlcView);
            applyFillScreen(); // 按设置应用画面缩放（VLC 直连：TextureView 等比放大裁剪）
        } else {
            playerView.setVisibility(View.VISIBLE);
            vlcView.setVisibility(View.GONE);
            core.attach(playerView);
            applyFillScreen(); // 按设置应用画面缩放（HLS/Media3 内核）
        }
    }

    /** 播放画面撑满屏幕缩放开关：Media3(HLS) 用 ZOOM 裁剪；VLC(直连) 用原生 SURFACE_FILL 等比放大裁剪。 */
    private void applyFillScreen() {
        try {
            boolean fill = getSharedPreferences(PREFS, MODE_PRIVATE).getBoolean(KEY_FILL_SCREEN, false);
            if (core == null) return;
            if (!"vlc".equals(coreKind)) {
                playerView.setResizeMode(fill
                        ? AspectRatioFrameLayout.RESIZE_MODE_ZOOM
                        : AspectRatioFrameLayout.RESIZE_MODE_FIT);
            } else {
                core.setFillScreen(fill);
            }
        } catch (Exception ignored) {
        }
    }

    private String hlsUrl(long sid) {
        return Api.base + "/hls/" + sid + "/master.m3u8";
    }

    /** 直连流 URL；startMs>0 时带起始位置参数（服务端 ffmpeg -ss 从该位置开始转码输出）。 */
    private String streamUrl(long sid, int track, long startMs) {
        String u = track >= 0
                ? (Api.base + "/stream/" + sid + "?track=" + track)
                : (Api.base + "/stream/" + sid);
        if (startMs > 0) {
            u += (u.contains("?") ? "&" : "?") + "start=" + startMs;
        }
        return u;
    }

    private String streamUrl(long sid, int track) {
        return streamUrl(sid, track, -1);
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
            volume = Math.max(0f, volume - 0.2f);
            core.setVolume(volume);
            tvInfo.setText("音量 " + Math.round(volume * 100) + "%");
            touch();
        });
        btnVolUp.setOnClickListener(v -> {
            volume = Math.min(1f, volume + 0.2f);
            core.setVolume(volume);
            tvInfo.setText("音量 " + Math.round(volume * 100) + "%");
            touch();
        });
        btnVoice.setOnClickListener(v -> {
            switchVoice("", true); // 本地按钮：切换并上报
            touch();
        });
        btnScan.setOnClickListener(v -> {
            // 扫码按钮：显示则隐藏、隐藏则显示（8s 临时）——toggle 语义独立于待机常显
            if (qrView.getVisibility() == View.VISIBLE) {
                qrView.setVisibility(View.GONE);
                ui.removeCallbacks(qrRunnable);
            } else {
                showQr(false);
            }
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

    private long lastRandomFillAt = 0;
    private boolean randomFilling = false;
    /** 随机播放"人在使用"的最后活跃时间：遥控/触摸/手机端消息/服务端新点歌任一发生即刷新；
     *  超过设置的空闲分钟数无任何活跃 → maybeRandomFill 自动关闭随机（防空转吃 CPU）。 */
    private volatile long lastActiveAt = System.currentTimeMillis();

    /** 随机空闲自动停止时长（分钟）：设置可配，0=不自动停止，默认 30。 */
    private int getRandomIdleMin() {
        return getSharedPreferences(PREFS, MODE_PRIVATE).getInt(KEY_RANDOM_IDLE_MIN, RANDOM_IDLE_DEFAULT_MIN);
    }

    /** 标记"有人在用"：任何本地/远端操作都刷新活跃时间，随机播放据此续播。 */
    private void markActive() {
        lastActiveAt = System.currentTimeMillis();
    }

    /** 随机播放：队列空时从服务端随机拿一首入队（仅 suiji 开启时调用，不写服务端设置）。
     * 手机端点歌台发 WS control random 置位 suiji；队列播空后由本端轮询随机补曲，
     * 实现持续随机播放；补曲入队广播后 pollState 拉到 playing 即进入播放。
     * 空闲保护（服务端零改动）：补曲前检测服务端 /api/player/state 的 queue，
     * waiting>0（有人点歌排队）刷新活跃时间；超过设置的空闲分钟数无任何活跃
     * （遥控/触摸/手机消息/新点歌均无）→ 自动 suiji=false 停止随机，释放服务端转码。 */
    private void maybeRandomFill() {
        if (!Boolean.TRUE.equals(suiji) || randomFilling) return;
        long now = System.currentTimeMillis();
        if (now - lastRandomFillAt < 4000) return; // 防抖：避免轮询空转重复入队
        lastRandomFillAt = now;
        randomFilling = true;
        exec.execute(() -> {
            try {
                // 检测服务端队列 waiting：有人在点歌排队 = 有人在用，刷新活跃时间
                try {
                    Api.HttpResult sr = Api.get("/api/player/state");
                    if (sr.code == 200) {
                        JSONArray q = new JSONObject(sr.body).optJSONArray("queue");
                        if (q != null) {
                            for (int i = 0; i < q.length(); i++) {
                                if ("waiting".equals(q.getJSONObject(i).optString("status"))) {
                                    lastActiveAt = System.currentTimeMillis();
                                    break;
                                }
                            }
                        }
                    }
                } catch (Exception ignored) {
                }
                // 空闲超时保护：超过设置分钟数无人使用 → 自动关闭随机（0=不自动停止）
                int idleMin = getRandomIdleMin();
                if (idleMin > 0 && now - lastActiveAt > idleMin * 60000L) {
                    suiji = false;
                    randomFilling = false;
                    Log.d(TAG, "random auto-stop: 空闲" + idleMin + "分钟无操作，自动关闭随机");
                    ui.post(() -> tvInfo.setText("随机播放已暂停（" + idleMin + "分钟无操作）"));
                    return; // 不再补曲
                }
                Api.HttpResult rs = Api.get("/api/songs");
                if (rs.code != 200) return;
                JSONArray arr;
                try {
                    arr = new JSONArray(rs.body);
                } catch (Exception e) {
                    arr = new JSONObject(rs.body).optJSONArray("data");
                }
                if (arr == null || arr.length() == 0) return;
                long pick = arr.getJSONObject((int) (Math.random() * arr.length())).optLong("id", -1);
                if (pick > 0) {
                    JSONObject b = new JSONObject();
                    b.put("song_id", pick);
                    b.put("nickname", "随机播放");
                    Api.post("/api/queue", b);
                    Log.d(TAG, "randomFill: pick=" + pick);
                }
            } catch (Exception ignored) {
            } finally {
                randomFilling = false;
            }
        });
    }

    /** 播放/暂停切换（本地按钮与控制端共用）。
     * userPaused 标记过 → 无条件恢复（VLC/Media3 的 resume 在播放中均无害忽略，
     * 避免 VLC 暂停后 isPlaying() 异步未更新导致误走同步分支而不恢复）；
     * 播放中 → 暂停；缓冲/未起播 → 也执行暂停意图（Media3 playWhenReady=false
     * 缓冲完自动停；VLC 直连起播快此分支罕见）。 */
    private void togglePlayLocal() {
        if (core == null) return;
        long now = System.currentTimeMillis();
        // 防抖：控制端一次点击偶尔会到达 2 条 play_pause（双端/连点），
        // 若不拦截会"暂停→立即恢复"打架；300ms 内只响应第一次。
        if (now - lastToggleAt < 300) {
            Log.d(TAG, "togglePlay: debounced");
            return;
        }
        lastToggleAt = now;
        Log.d(TAG, "togglePlay: userPaused=" + userPaused + " playing=" + core.isPlaying() + " kind=" + coreKind);
        if (userPaused) {
            core.resume();
            userPaused = false;
            btnPlay.setText("暂停");
        } else if (core.isPlaying()) {
            core.pause();
            userPaused = true;
            btnPlay.setText("播放");
        } else {
            // 缓冲/未起播：执行暂停意图
            core.pause();
            userPaused = true;
            btnPlay.setText("播放");
        }
        sendState();
    }

    /** 上报播放状态给控制端（手机/平板按钮图标据此实时同步，避免"按钮状态与 TV 实际不符"）。 */
    private void sendState() {
        if (ctrlWS == null) return;
        try {
            JSONObject o = new JSONObject();
            o.put("type", "state");
            o.put("paused", userPaused);
            o.put("voice", voiceOriginal ? "original" : "accompaniment");
            ctrlWS.send(o.toString());
        } catch (Exception ignored) {
        }
    }

    /** 重唱：回到歌曲开头重新播放（本地按钮与控制端共用）。 */
    private void repeatLocal() {
        if (core != null) {
            core.seekTo(0);
            core.resume();
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

            @Override
            public void onQueueChanged() {
                // 点歌/置顶/删歌等队列变化：立即刷新状态，避免字幕"下一曲"信息滞后
                pollState();
            }
        });
        ctrlWS.connect();
    }

    /** 氛围互动：播放原生音效 + 顶部浮层。 */
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
        playEffectSfx(resId, volume);
    }

    /** 浮层：顶部居中弹出，2.6 秒后消失。 */
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
        markActive(); // 收到手机端指令 = 有人在用
        Log.d(TAG, "handleControl: " + action);
        switch (action) {
            case "play_pause":
                togglePlayLocal();
                break;
            case "voice":
                // 只响应与当前播放歌曲匹配的切音轨指令（避免网页/其他播放端广播干扰）。
                // 广播指令只执行切换、不回发上报（见 switchVoice fromUser 参数），
                // 避免"广播→上报→再广播"死循环导致疯狂切伴奏。
                long vsid = msg.optLong("song_id", -1);
                if (vsid <= 0 || vsid == playingSongId) {
                    switchVoice(msg.optString("to", ""), false);
                }
                break;
            case "repeat":
                repeatLocal();
                break;
            case "next":
                postNext();
                break;
            case "volume_up":
                volume = Math.min(1f, volume + 0.1f);
                core.setVolume(volume);
                break;
            case "volume_down":
                volume = Math.max(0f, volume - 0.1f);
                core.setVolume(volume);
                break;
            case "eq":
                // 控制端均衡器：{action:'eq', gains:[10段dB], preset}
                JSONArray eqa = msg.optJSONArray("gains");
                if (eqa != null && eqa.length() > 0) {
                    float[] gains = new float[eqa.length()];
                    for (int i = 0; i < eqa.length(); i++) {
                        gains[i] = (float) eqa.optDouble(i, 0);
                    }
                    lastEqGains = gains;
                    if (core != null) core.setEqualizer(gains);
                    Log.d(TAG, "eq applied: " + gains.length + " bands, preset=" + msg.optString("preset", ""));
                }
                break;
            case "random":
                // 手机端点歌台随机开关：{action:'random', on:true/false} → 本地临时置位（不保存）
                suiji = msg.optBoolean("on", !Boolean.TRUE.equals(suiji));
                Log.d(TAG, "suiji=" + suiji);
                break;
            default:
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
                long nid = -1;
                if (playing != null) {
                    sid = playing.optLong("song_id");
                    title = playing.optString("title", "");
                    artist = playing.optString("artist", "");
                }
                JSONObject next = data.optJSONObject("next");
                if (next != null) {
                    nid = next.optLong("song_id", -1);
                    ntitle = next.optString("title", "");
                    nartist = next.optString("artist", "");
                }
                final long fSid = sid;
                final String fTitle = title, fArtist = artist, fNTitle = ntitle, fNArtist = nartist;
                final long fNid = nid;
                ui.post(() -> {
                    applyFillScreen(); // 每轮轮询应用画面缩放：VLC 视频尺寸就绪后自动生效
                    onState(fSid, fTitle, fArtist, fNTitle, fNArtist, fNid);
                });
            } catch (Exception e) {
                ui.post(() -> tvInfo.setText("服务器连接中…"));
            }
        });
    }

    private void onState(long sid, String title, String artist, String ntitle, String nartist, long nid) {
        Log.d(TAG, "onState: sid=" + sid + " cur=" + playingSongId
                + " corePlaying=" + (core != null && core.isPlaying()) + " userPaused=" + userPaused
                + " hlsEver=" + hlsEverPlayed);
        // 轮询检测到真正播放 → 标记"已播放过"（直连 watchdog 不再回退 HLS，HLS 看门狗不再重载）
        if (core != null && core.isPlaying()) {
            directEverPlayed = true;
            hlsEverPlayed = true;
            cancelHlsWatchdog();
        }
        nowPlaying = title;
        nowArtist = artist;
        nextSong = ntitle;
        nextArtist = nartist;
        nextSongId = nid;
        if (sid <= 0) {
            // 无播放歌曲（未点歌 / 切完最后一首）：随机开关开启则自动随机补一首，
            // 不依赖服务端 autoplay_random 设置（临时开关不保存，纯客户端行为）。
            if (Boolean.TRUE.equals(suiji)) {
                maybeRandomFill();
            }
            // 无播放歌曲（未点歌 / 切完最后一首且不随机）：显示待机大屏并停止播放
            showIdle();
            tvInfo.setText("请点歌");
            playingSongId = -1;
            btnPlay.setText("暂停");
            cancelDirectWatchdog();
            cancelHlsWatchdog();
            if (core != null) {
                core.stop();
            }
            return;
        }
        hideIdle();
        tvInfo.setText("正在播放：" + (title.isEmpty() ? "请点歌" : title + " - " + artist));
        if (sid != playingSongId) {
            Log.d(TAG, "PLAY_SONG: sid=" + sid + " reason=state-change hlsFallback=" + hlsFallback);
            playingSongId = sid;
            hlsFallback = false;
            userPaused = false;
            lastPreloadSongId = -1;
            playerErrorCount = 0;
            btnPlay.setText("暂停");
            playSong(sid);
        } else if (core != null) {
            // 播放超过 30s：预热下一首转码缓存（切歌秒开）        、
            try {
                String mode = playMode();
                // 仅 HLS 模式 / HLS自动回退场景执行预加载；direct直连模式跳过预加载
                if ((!"direct".equals(mode) || hlsFallback)
                        && nid > 0 && nid != lastPreloadSongId && core.getPosition() > PRELOAD_POSITION_MS) {
                    lastPreloadSongId = nid;
                    preloadSong(nid);
                }
            } catch (Exception ignored) {
            }
        }
        }

    /** 按当前模式播放：HLS(Media3) 或 直连(directCore，默认 VLC)。 */
    private void playSong(long sid) {
        if ("hls".equals(playMode()) || hlsFallback) {
            ensureCore("media3");
            // 先应用音轨默认再起播：Media3 TrackSelector 起播即目标音轨，避免起播后再切换
            applyVoiceDefault();
            core.play(hlsUrl(sid));
            cancelDirectWatchdog();
            // HLS 起播看门狗：卡 BUFFERING 不报错，超时自动重载/切歌，避免永久转圈
            hlsEverPlayed = false;
            hlsWatchdogTries = 0;
            startHlsWatchdog();
        } else {
            ensureCore(directCore());
            currentStreamOffsetMs = 0;
            directEverPlayed = false; // 新歌直连：重置"曾播放"标志
            // 先应用音轨默认再起播：VLC 用 :audio-track 预选目标音轨，起播即目标音轨（不转码、不后切）
            applyVoiceDefault();
            core.play(streamUrl(sid, -1));
            startDirectWatchdog();
        }
        sendState(); // 切歌起播后同步状态给控制端（paused=false）
        if (lastEqGains != null) core.setEqualizer(lastEqGains); // 切内核/新歌后重应用均衡器
    }

    /** 直连失败/不支持 → 自动回退 HLS(Media3)，同歌续播。 */
    private void fallbackToHls(String msg) {
        hlsFallback = true;
        playerErrorCount = 0;
        cancelDirectWatchdog();
        tvInfo.setText(msg);
        ensureCore("media3");
        long sid = playingSongId;
        if (sid > 0) {
            applyVoiceDefault();
            core.play(hlsUrl(sid));
            hlsEverPlayed = false;
            hlsWatchdogTries = 0;
            startHlsWatchdog();
        }
    }

    private void startHlsWatchdog() {
        ui.removeCallbacks(hlsWatchdogRunnable);
        ui.postDelayed(hlsWatchdogRunnable, HLS_WATCHDOG_MS);
    }

    private void cancelHlsWatchdog() {
        ui.removeCallbacks(hlsWatchdogRunnable);
    }

    private void startDirectWatchdog() {
        ui.removeCallbacks(directWatchdogRunnable);
        ui.postDelayed(directWatchdogRunnable, DIRECT_WATCHDOG_MS);
    }

    private void cancelDirectWatchdog() {
        ui.removeCallbacks(directWatchdogRunnable);
    }

    private void preloadSong(long songId) {
        exec.execute(() -> {
            try {
                JSONObject b = new JSONObject();
                b.put("song_id", songId);
                Api.post("/api/preload", b);
            } catch (Exception ignored) {
            }
        });
    }

    /** 显示待机大屏（仿网页 TV：家悦K歌 logo + 播放端已就绪），并默认显示右上角点歌二维码。 */
    private void showIdle() {
        if (idleView != null && idleView.getVisibility() != View.VISIBLE) {
            applyLogoGradient();
            idleView.setVisibility(View.VISIBLE);
        }
        showQr(true); // 无人点歌：右上角二维码常显
        // 滚动字幕避开右上角二维码区域（只滚到二维码左侧）
        if (tickerHost != null) {
            tickerHost.setPadding(tickerHost.getPaddingLeft(), tickerHost.getPaddingTop(), dp(190), tickerHost.getPaddingBottom());
        }
    }

    private void hideIdle() {
        if (idleView != null && idleView.getVisibility() == View.VISIBLE) {
            idleView.setVisibility(View.GONE);
        }
        // 进入播放：隐藏待机二维码
        if (qrView.getVisibility() == View.VISIBLE) {
            qrView.setVisibility(View.GONE);
            ui.removeCallbacks(qrRunnable);
        }
        // 播放时滚动字幕恢复满宽
        if (tickerHost != null) {
            tickerHost.setPadding(tickerHost.getPaddingLeft(), tickerHost.getPaddingTop(), 0, tickerHost.getPaddingBottom());
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
    private long lastNextAt = 0;

    private void postNext() {
        Log.d(TAG, "postNext: from=" + Thread.currentThread().getStackTrace()[2].getMethodName());
        long now = System.currentTimeMillis();
        if (now - lastNextAt < 800) return; // 防抖：800ms 内重复切歌忽略（防遥控连点跳多首）
        lastNextAt = now;
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

    /**
     * 原唱/伴唱切换。to 为空=按钮翻转；to=original/accompaniment=按目标切（控制端指令）。
     * fromUser=true（本地按钮）→ 切换并上报服务端；fromUser=false（WS 广播指令）→
     * 只执行切换、不回发上报，避免"广播→上报→再广播"死循环（疯狂切伴奏）。
     * HLS 走 TrackSelector 原位无缝切换；直连(VLC) 走源文件双音轨原生切换（不转码、不换内核）。
     */
    private void switchVoice(String to, boolean fromUser) {
        // 远程广播防抖：控制端多端/多连接重复点击时，一次操作会到达多条 voice
        //（实测一次点击 4 条、2ms 内），而"切换"是翻转语义，多条会互相抵消
        // → 表现为"切不了"。500ms 内只响应第一条远程指令，本地按键不受限。
        if (!fromUser) {
            long now = System.currentTimeMillis();
            if (now - lastVoiceAt < 500) {
                Log.d(TAG, "switchVoice: debounced");
                return;
            }
            lastVoiceAt = now;
        }
        voiceSpecified = true; // 用户手动指定过音轨，「上次」模式据此沿用
        Log.d(TAG, "switchVoice: to=" + to + " fromUser=" + fromUser + " cur=" + (voiceOriginal ? "original" : "accompaniment"));
        int target;
        if ("accompaniment".equals(to)) {
            target = 1;
            voiceOriginal = false;
        } else if ("original".equals(to)) {
            target = 0;
            voiceOriginal = true;
        } else {
            target = voiceOriginal ? 1 : 0; // 按钮翻转
            voiceOriginal = !voiceOriginal;
        }
        btnVoice.setText(voiceOriginal ? "伴唱" : "原唱");

        // 音轨切换统一走内核原生切换，不换内核、不转码：
        // HLS(Media3) → TrackSelector 原位无缝切换；直连(VLC) → 源文件双音轨原生 setAudioTrack。
        core.setAudioTrack(target);

        if (fromUser) {
            reportVoiceSwitch(target);
        }
        sendState(); // 同步音轨状态给控制端（手机端原/伴唱高亮）
    }

    /** 上报音轨切换给服务端（仅本地用户操作时调用）。 */
    private void reportVoiceSwitch(int target) {
        long sid = playingSongId;
        if (sid <= 0) return;
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

    /** 应用音轨：统一走内核原生切换（HLS→TrackSelector；直连→VLC 原生，服务端不转码）。 */
    private void applyVoiceTrack(int track) {
        if (core == null || playingSongId <= 0) return;
        core.setAudioTrack(track);
    }

    /** 切歌默认请求：新歌加载后按设置应用音轨。
     *  伴唱(accompaniment)：一律切伴唱；原唱(original)：一律原唱；
     *  上次(last)：沿用上一首的音轨，上一首没被手动指定过则默认伴奏。 */
    private void applyVoiceDefault() {
        String def = getSharedPreferences(PREFS, MODE_PRIVATE).getString(KEY_VOICE_DEFAULT, "last");
        if ("original".equals(def)) {
            voiceOriginal = true;
            btnVoice.setText("伴唱");
            applyVoiceTrack(0);
        } else if ("accompaniment".equals(def)) {
            voiceOriginal = false;
            btnVoice.setText("原唱");
            applyVoiceTrack(1);
        } else { // last
            if (voiceSpecified) {
                // 上一首被指定过：沿用其音轨
                if (!voiceOriginal) applyVoiceTrack(1);
            } else {
                // 上一首没指定：默认伴奏
                voiceOriginal = false;
                btnVoice.setText("原唱");
                applyVoiceTrack(1);
            }
        }
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
        markActive(); // 屏幕点击 = 有人在用
        if (controlBar.getVisibility() != View.VISIBLE) showControls();
        else resetHideTimer();
    }

    @Override
    public boolean dispatchKeyEvent(KeyEvent event) {
        if (event.getAction() == KeyEvent.ACTION_DOWN) {
            markActive(); // 遥控按键 = 有人在用
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
                    if (controlBar.getVisibility() != View.VISIBLE) {
                        showControls();
                        return true;
                    }
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

    /** 扫码：显示则隐藏，隐藏则显示（右上角二维码）。
     * persistent=true 常显（待机大屏默认显示，不自动隐藏，已显示时不重建——避免闪烁）；
     * false 为点按临时显示，8s 后自动隐藏。 */
    private void showQr(boolean persistent) {
        if (qrView.getVisibility() == View.VISIBLE) {
            // 已显示：不 toggle、不重建（showIdle 反复调用不再导致一闪一闪）
            ui.removeCallbacks(qrRunnable);
            if (!persistent) {
                ui.postDelayed(qrRunnable, QR_DURATION);
            }
            return;
        }
        if (Api.base.isEmpty()) return;
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
            if (!persistent) {
                ui.postDelayed(qrRunnable, QR_DURATION);
            }
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

    /** 字幕跑完一段后：先接欢迎语段，全部播完再等随机间隔出现。 */
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

    // ---------- 设置 ----------

    /** 设置页：服务器地址 + 播放模式(HLS 默认) + 播完随机播放 + 切歌默认请求。
     *  直连模式固定 VLC 内核，HLS 固定 Media3。 */
    private void showSettingsDialog() {
        SharedPreferences sp = getSharedPreferences(PREFS, MODE_PRIVATE);
        String cur = sp.getString(KEY_SERVER, "");

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        int pad = dp(24);
        root.setPadding(pad, pad / 2, pad, 0);
        ScrollView sv = new ScrollView(this);
        sv.setFillViewport(true);
        sv.setFocusable(false); // TV 遥控：ScrollView 不抢焦点，方向键直接进入内部控件（否则焦点会跳到对话框按钮、跳过单选组）
        sv.addView(root);

        // 服务器地址
        TextView t1 = new TextView(this);
        t1.setText("服务器地址（IP:端口）");
        t1.setTextColor(Color.WHITE);
        EditText et = new EditText(this);
        et.setHint("例如 192.168.1.100:8086");
        et.setHintTextColor(0xFF8A8AA3);
        et.setTextColor(Color.WHITE);
        et.setText(cur.replace("http://", ""));
        root.addView(t1);
        root.addView(et);

        // 扫描服务端（端口默认 8086）
        LinearLayout scanRow = new LinearLayout(this);
        scanRow.setGravity(Gravity.CENTER_VERTICAL);
        scanRow.setOrientation(LinearLayout.HORIZONTAL);
        EditText etPort = new EditText(this);
        etPort.setHint("端口");
        etPort.setHintTextColor(0xFF8A8AA3);
        etPort.setTextColor(Color.WHITE);
        etPort.setText("8086");
        etPort.setInputType(android.text.InputType.TYPE_CLASS_NUMBER);
        etPort.setLayoutParams(new LinearLayout.LayoutParams(dp(90), LinearLayout.LayoutParams.WRAP_CONTENT));
        Button btnScan = new Button(this);
        btnScan.setText("扫描服务端");
        btnScan.setAllCaps(false);
        btnScan.setTextColor(Color.WHITE);
        btnScan.setBackgroundResource(R.drawable.focus_bg); // 遥控聚焦高亮（青边深底）
        btnScan.setFocusable(true);
        scanRow.addView(etPort);
        scanRow.addView(btnScan);
        root.addView(scanRow);
        final String[] scanning = {null};
        btnScan.setOnClickListener(v -> {
            if (scanning[0] != null) return;
            int port;
            try {
                port = Integer.parseInt(etPort.getText().toString().trim());
            } catch (Exception ex) {
                port = 8086;
            }
            scanning[0] = "1";
            btnScan.setText("扫描中…");
            scanServers(port, results -> {
                scanning[0] = null;
                btnScan.setText("扫描服务端");
                if (results.isEmpty()) {
                    Toast.makeText(this, "未发现可用的服务端", Toast.LENGTH_SHORT).show();
                    return;
                }
                String[] arr = results.toArray(new String[0]);
                new AlertDialog.Builder(this)
                        .setTitle("发现服务端")
                        .setItems(arr, (d, w) -> et.setText(arr[w].replace("http://", "")))
                        .setNegativeButton("取消", null)
                        .show();
            });
        });

        // 播放画面撑满屏幕（HLS/Media3 内核）
        LinearLayout rowFill = new LinearLayout(this);
        rowFill.setGravity(Gravity.CENTER_VERTICAL);
        rowFill.setOrientation(LinearLayout.HORIZONTAL);
        rowFill.setClickable(true);
        rowFill.setFocusable(true);
        rowFill.setPadding(0, dp(6), 0, dp(6));
        TextView tFill = new TextView(this);
        tFill.setText("播放画面撑满屏幕");
        tFill.setTextColor(Color.WHITE);
        LinearLayout.LayoutParams lpFill = new LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f);
        tFill.setLayoutParams(lpFill);
        SwitchCompat swFill = new SwitchCompat(this);
        swFill.setClickable(true);
        swFill.setFocusable(true);
        // 开关配色：选中亮青轨道/白拇指，未选浅紫灰轨道/浅白拇指（深蓝紫背景上清晰可见）
        int[][] swStates = new int[][]{new int[]{android.R.attr.state_checked}, new int[]{}};
        swFill.setTrackTintList(new android.content.res.ColorStateList(swStates,
                new int[]{0xFF2FC7EF, 0xFF4A4F73}));
        swFill.setThumbTintList(new android.content.res.ColorStateList(swStates,
                new int[]{0xFFFFFFFF, 0xFFC6CAE4}));
        rowFill.addView(tFill);
        rowFill.addView(swFill);
        root.addView(rowFill);
        rowFill.setBackgroundResource(R.drawable.focus_bg); // 行聚焦高亮（遥控操作开关时可见）
        rowFill.setOnClickListener(v -> swFill.toggle());
        swFill.setChecked(sp.getBoolean(KEY_FILL_SCREEN, false));

        // 切歌默认请求：伴唱 / 原唱 / 上次
        TextView t3 = new TextView(this);
        t3.setText("切歌默认请求");
        t3.setTextColor(Color.WHITE);
        t3.setPadding(0, dp(10), 0, 0);
        root.addView(t3);
        RadioGroup rg = new RadioGroup(this);
        rg.setOrientation(RadioGroup.HORIZONTAL);
        RadioButton rbAcc = new RadioButton(this);
        rbAcc.setText("伴唱");
        rbAcc.setTextColor(Color.WHITE);
        rbAcc.setButtonDrawable(R.drawable.radio_btn);
        RadioButton rbOrig = new RadioButton(this);
        rbOrig.setText("原唱");
        rbOrig.setTextColor(Color.WHITE);
        rbOrig.setButtonDrawable(R.drawable.radio_btn);
        RadioButton rbLast = new RadioButton(this);
        rbLast.setText("上次");
        rbLast.setTextColor(Color.WHITE);
        rbLast.setButtonDrawable(R.drawable.radio_btn);
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

        // 播放模式：HLS（默认）/ 直连
        TextView tMode = new TextView(this);
        tMode.setText("播放模式");
        tMode.setTextColor(Color.WHITE);
        tMode.setPadding(0, dp(10), 0, 0);
        root.addView(tMode);
        RadioGroup rgMode = new RadioGroup(this);
        rgMode.setOrientation(RadioGroup.HORIZONTAL);
        RadioButton rbHls = new RadioButton(this);
        rbHls.setText("HLS");
        rbHls.setTextColor(Color.WHITE);
        rbHls.setButtonDrawable(R.drawable.radio_btn);
        RadioButton rbDirect = new RadioButton(this);
        rbDirect.setText("直连");
        rbDirect.setTextColor(Color.WHITE);
        rbDirect.setButtonDrawable(R.drawable.radio_btn);
        rbHls.setPadding(dp(8), 0, dp(8), 0);
        rgMode.addView(rbHls);
        rgMode.addView(rbDirect);
        root.addView(rgMode);
        String pm = sp.getString(KEY_PLAY_MODE, "hls");
        if ("hls".equals(pm)) rbHls.setChecked(true);
        else rbDirect.setChecked(true);

        // 单选状态样式：选中=亮青底+白字；焦点未选=白色高亮框+白字（遥控光标）；普通未选=透明+浅紫灰字
        android.widget.RadioGroup.OnCheckedChangeListener paint = (g, id) -> {
            for (int i = 0; i < g.getChildCount(); i++) {
                RadioButton r = (RadioButton) g.getChildAt(i);
                r.setBackgroundResource(r.isChecked() ? R.drawable.bg_checked_option
                        : (r.hasFocus() ? R.drawable.bg_option_focused : 0));
                r.setTextColor(r.isChecked() ? 0xFFFFFFFF
                        : (r.hasFocus() ? 0xFFFFFFFF : 0xFFB9BEDD));
            }
        };
        rg.setOnCheckedChangeListener(paint);
        rgMode.setOnCheckedChangeListener(paint);
        paint.onCheckedChanged(rg, rg.getCheckedRadioButtonId());
        paint.onCheckedChanged(rgMode, rgMode.getCheckedRadioButtonId());

        // 随机空闲自动停止（0=关闭，默认30分钟）
        TextView tIdle = new TextView(this);
        tIdle.setText("随机空闲自动停止");
        tIdle.setTextColor(Color.WHITE);
        tIdle.setPadding(0, dp(10), 0, 0);
        root.addView(tIdle);
        RadioGroup rgIdle = new RadioGroup(this);
        rgIdle.setOrientation(RadioGroup.HORIZONTAL);
        final int[] idleOptions = {0, 15, 30, 60};
        final String[] idleLabels = {"关闭", "15分钟", "30分钟", "60分钟"};
        final RadioButton[] idleRbs = new RadioButton[idleOptions.length];
        for (int i = 0; i < idleOptions.length; i++) {
            RadioButton rb = new RadioButton(this);
            rb.setText(idleLabels[i]);
            rb.setTextColor(Color.WHITE);
            rb.setButtonDrawable(R.drawable.radio_btn);
            rb.setPadding(dp(8), 0, dp(8), 0);
            rgIdle.addView(rb);
            idleRbs[i] = rb;
        }
        root.addView(rgIdle);
        int curIdle = sp.getInt(KEY_RANDOM_IDLE_MIN, RANDOM_IDLE_DEFAULT_MIN);
        int idleIdx = 2; // 默认 30 分钟
        for (int i = 0; i < idleOptions.length; i++) {
            if (idleOptions[i] == curIdle) idleIdx = i;
        }
        idleRbs[idleIdx].setChecked(true);
        rgIdle.setOnCheckedChangeListener(paint);
        paint.onCheckedChanged(rgIdle, rgIdle.getCheckedRadioButtonId());

        // 单选焦点反馈：遥控光标移动时白色高亮框跟随（TV 遥控操作，光标可见）
        android.view.View.OnFocusChangeListener focusPaint = (v, has) -> {
            RadioButton r = (RadioButton) v;
            paint.onCheckedChanged((android.widget.RadioGroup) r.getParent(), r.getId());
        };
        for (android.widget.RadioGroup g : new android.widget.RadioGroup[]{rg, rgMode, rgIdle}) {
            for (int i = 0; i < g.getChildCount(); i++) {
                g.getChildAt(i).setFocusable(true);
                g.getChildAt(i).setOnFocusChangeListener(focusPaint);
            }
        }

        // 直连回退提示
        TextView tNote = new TextView(this);
        tNote.setText("直连播放失败或不受支持时会自动切换 HLS");
        tNote.setPadding(0, dp(8), 0, 0);
        tNote.setTextSize(12);
        tNote.setTextColor(0xFF8A8AA3);
        root.addView(tNote);

        // 输入框聚焦高亮：深色背景上清晰显示光标/焦点位置（TV 遥控操作）
        android.graphics.drawable.GradientDrawable etFocus = new android.graphics.drawable.GradientDrawable();
        etFocus.setShape(android.graphics.drawable.GradientDrawable.RECTANGLE);
        etFocus.setColor(0xFF2A2A3C);
        etFocus.setCornerRadius(dp(8));
        etFocus.setStroke(dp(2), 0xFF36D9F7);
        android.graphics.drawable.GradientDrawable etNorm = new android.graphics.drawable.GradientDrawable();
        etNorm.setShape(android.graphics.drawable.GradientDrawable.RECTANGLE);
        etNorm.setColor(0xFF23232F);
        etNorm.setCornerRadius(dp(8));
        etNorm.setStroke(dp(1), 0xFF55556A);
        android.graphics.drawable.StateListDrawable etBg = new android.graphics.drawable.StateListDrawable();
        etBg.addState(new int[]{android.R.attr.state_focused}, etFocus);
        etBg.addState(new int[]{}, etNorm);
        et.setBackground(etBg);
        etPort.setBackground(etBg);

        android.app.AlertDialog dialog = new AlertDialog.Builder(this, R.style.Theme_Jiayuektv_Dialog)
                .setTitle("设置")
                .setView(sv)
                .setNegativeButton("取消", null)
                .setPositiveButton("保存", (d, w) -> {
                    String v = et.getText().toString().trim().replace("http://", "");
                    int rid = rg.getCheckedRadioButtonId();
                    String vd = rid == rbAcc.getId() ? "accompaniment"
                            : rid == rbOrig.getId() ? "original" : "last";
                    String newMode = rgMode.getCheckedRadioButtonId() == rbHls.getId() ? "hls" : "direct";
                    String oldMode = sp.getString(KEY_PLAY_MODE, "hls");
                    int idleVal = idleOptions[0];
                    for (int i = 0; i < idleRbs.length; i++) {
                        if (idleRbs[i].isChecked()) idleVal = idleOptions[i];
                    }
                    sp.edit()
                            .putString(KEY_VOICE_DEFAULT, vd)
                            .putString(KEY_PLAY_MODE, newMode)
                            .putBoolean(KEY_FILL_SCREEN, swFill.isChecked())
                            .putInt(KEY_RANDOM_IDLE_MIN, idleVal)
                            .putInt(KEY_CFG_VER, CFG_VER_CURRENT)
                            .apply();
                    applyFillScreen(); // 立即应用画面缩放
                    if (!v.isEmpty()) {
                        String url = v.startsWith("http") ? v : "http://" + v;
                        sp.edit().putString(KEY_SERVER, url).apply();
                        Api.base = url;
                        restartPlayback();
                        Toast.makeText(this, "已保存，连接 " + url, Toast.LENGTH_SHORT).show();
                    } else if (!newMode.equals(oldMode) && playingSongId > 0) {
                        // 播放模式已切换：立即按新模式重播当前歌（切内核），
                        // 避免"设置已切但当前歌还在旧内核播放"的控制错位
                        userPaused = false;
                        hlsFallback = false;
                        playerErrorCount = 0;
                        btnPlay.setText("暂停");
                        playSong(playingSongId);
                    }
                })
                .setCancelable(true)
                .create();
        dialog.show();
        // TV 遥控：聚焦地址框但隐藏软键盘（打开设置默认焦点在顶部，方向键从地址框开始导航）
        // 注意：show() 后 view 尚未 attach，直接 requestFocus() 会失败导致焦点落到取消按钮，
        // 必须等 onShow 回调（此时 view 已 attach）再请求焦点
        dialog.getWindow().setSoftInputMode(android.view.WindowManager.LayoutParams.SOFT_INPUT_STATE_HIDDEN);
        dialog.setOnShowListener(d -> et.requestFocus());
        // 输入框：方向键上下强制跳焦点（TV 遥控不乱跳），左右保留光标编辑
        android.view.View.OnKeyListener editNav = (v, keyCode, event) -> {
            if (event.getAction() == android.view.KeyEvent.ACTION_DOWN) {
                if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_DOWN) {
                    android.view.View f = v.focusSearch(android.view.View.FOCUS_DOWN);
                    if (f != null) f.requestFocus();
                    return true;
                }
                if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_UP) {
                    android.view.View f = v.focusSearch(android.view.View.FOCUS_UP);
                    if (f != null) f.requestFocus();
                    return true;
                }
                if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_RIGHT) {
                    int p = ((android.widget.EditText) v).getSelectionStart();
                    ((android.widget.EditText) v).setSelection(Math.min(p + 1, ((android.widget.EditText) v).length()));
                    return true;
                }
                if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_LEFT) {
                    int p = ((android.widget.EditText) v).getSelectionStart();
                    ((android.widget.EditText) v).setSelection(Math.max(p - 1, 0));
                    return true;
                }
            }
            return false;
        };
        et.setOnKeyListener(editNav);
        etPort.setOnKeyListener(editNav);
        // 深蓝紫渐变背景（不再是纯黑），与主界面深色基调衔接但明显更"彩色"
        android.graphics.drawable.GradientDrawable dlgBg = new android.graphics.drawable.GradientDrawable(
                android.graphics.drawable.GradientDrawable.Orientation.TL_BR,
                new int[]{0xFF3A3F6E, 0xFF252A48});
        dialog.getWindow().getDecorView().setBackground(dlgBg);
        // 保存/取消按钮：主次配色（青渐变 = 保存，灰紫渐变 = 取消）
        try {
            Button bPos = dialog.getButton(android.app.AlertDialog.BUTTON_POSITIVE);
            Button bNeg = dialog.getButton(android.app.AlertDialog.BUTTON_NEGATIVE);
            bPos.setBackgroundResource(R.drawable.bg_btn_primary);
            bNeg.setBackgroundResource(R.drawable.bg_btn_secondary);
            bPos.setTextColor(0xFF002C3D);
            bNeg.setTextColor(Color.WHITE);
            bPos.setAllCaps(false);
            bNeg.setAllCaps(false);
        } catch (Exception ignored) {
        }

        // ===== TV 遥控焦点顺序：纵向主链 + 行内横向，方向键不乱跳 =====
        int idAddr = View.generateViewId(); et.setId(idAddr);
        int idPortV = View.generateViewId(); etPort.setId(idPortV);
        int idScanV = View.generateViewId(); btnScan.setId(idScanV);
        int idFillV = View.generateViewId(); rowFill.setId(idFillV);
        int idFillSw = View.generateViewId(); swFill.setId(idFillSw);
        int idAccV = View.generateViewId(); rbAcc.setId(idAccV);
        int idOrigV = View.generateViewId(); rbOrig.setId(idOrigV);
        int idLastV = View.generateViewId(); rbLast.setId(idLastV);
        int idHlsV = View.generateViewId(); rbHls.setId(idHlsV);
        int idDirectV = View.generateViewId(); rbDirect.setId(idDirectV);
        int[] idleIds = new int[4];
        for (int i = 0; i < 4; i++) { idleIds[i] = View.generateViewId(); idleRbs[i].setId(idleIds[i]); }
        Button bCancelV = dialog.getButton(android.app.AlertDialog.BUTTON_NEGATIVE);
        Button bSaveV = dialog.getButton(android.app.AlertDialog.BUTTON_POSITIVE);
        int idCancelV = View.generateViewId(); bCancelV.setId(idCancelV);
        int idSaveV = View.generateViewId(); bSaveV.setId(idSaveV);

        // 下：纵向主链（地址→端口→撑满→伴唱→HLS→关闭→取消），保存循环回顶部
        et.setNextFocusDownId(idPortV);
        etPort.setNextFocusDownId(idFillV);
        btnScan.setNextFocusDownId(idFillV);
        rowFill.setNextFocusDownId(idAccV);
        swFill.setNextFocusDownId(idAccV);
        rbAcc.setNextFocusDownId(idHlsV);
        rbOrig.setNextFocusDownId(idDirectV);
        rbLast.setNextFocusDownId(idDirectV);
        rbHls.setNextFocusDownId(idleIds[0]);
        rbDirect.setNextFocusDownId(idleIds[1]);
        for (int i = 0; i < 4; i++) idleRbs[i].setNextFocusDownId(idCancelV);
        // 弹窗底部按钮按"下"直接循环回顶部地址框（取消→保存是同行右移，会让用户误以为“下=右”）
        bCancelV.setNextFocusDownId(idAddr);
        bSaveV.setNextFocusDownId(idAddr);

        // 上：反向主链
        et.setNextFocusUpId(idSaveV);
        etPort.setNextFocusUpId(idAddr);
        btnScan.setNextFocusUpId(idAddr);
        rowFill.setNextFocusUpId(idPortV);
        swFill.setNextFocusUpId(idPortV);
        rbAcc.setNextFocusUpId(idFillV);
        rbOrig.setNextFocusUpId(idFillV);
        rbLast.setNextFocusUpId(idFillV);
        rbHls.setNextFocusUpId(idAccV);
        rbDirect.setNextFocusUpId(idLastV);
        for (int i = 0; i < 4; i++) idleRbs[i].setNextFocusUpId(i == 0 ? idHlsV : idDirectV);
        bCancelV.setNextFocusUpId(idleIds[3]);
        bSaveV.setNextFocusUpId(idAddr);

        // 左/右：行内横向（组内相邻）
        etPort.setNextFocusRightId(idScanV);
        btnScan.setNextFocusLeftId(idPortV);
        rbAcc.setNextFocusRightId(idOrigV);
        rbOrig.setNextFocusLeftId(idAccV);
        rbOrig.setNextFocusRightId(idLastV);
        rbLast.setNextFocusLeftId(idOrigV);
        rbHls.setNextFocusRightId(idDirectV);
        rbDirect.setNextFocusLeftId(idHlsV);
        for (int i = 0; i < 4; i++) {
            if (i < 3) idleRbs[i].setNextFocusRightId(idleIds[i + 1]);
            if (i > 0) idleRbs[i].setNextFocusLeftId(idleIds[i - 1]);
        }
        bCancelV.setNextFocusRightId(idSaveV);
        bSaveV.setNextFocusLeftId(idCancelV);

        // ===== 方向语义强制：上下=纵向、左右=横向；跨行乱跳（右=下/上=右）被拦截 =====
        android.view.View.OnKeyListener dirNav = (v, keyCode, event) -> {
            if (event.getAction() != android.view.KeyEvent.ACTION_DOWN) return false;
            int dir;
            boolean isHoriz;
            if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_UP) { dir = android.view.View.FOCUS_UP; isHoriz = false; }
            else if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_DOWN) { dir = android.view.View.FOCUS_DOWN; isHoriz = false; }
            else if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_LEFT) { dir = android.view.View.FOCUS_LEFT; isHoriz = true; }
            else if (keyCode == android.view.KeyEvent.KEYCODE_DPAD_RIGHT) { dir = android.view.View.FOCUS_RIGHT; isHoriz = true; }
            else return false;
            android.view.View t = v.focusSearch(dir);
            if (t == null || t == v || !t.isFocusable()) return true;
            int[] a = new int[2]; int[] b = new int[2];
            v.getLocationOnScreen(a); t.getLocationOnScreen(b);
            if (isHoriz) {
                // 左右：目标必须与当前水平带重叠（同行），否则不跳（防止“右=下跳”）
                boolean sameRow = (b[1] < a[1] + v.getHeight()) && (a[1] < b[1] + t.getHeight());
                if (!sameRow) return true;
            } else {
                // 上下：用控件中心比较方向（同行也算“方向正确”，避免同 y 的取消/保存被误拦；
                // 只拦真正“往反方向跳”的情况：按下时目标中心在上方 / 按上时目标中心在下方）
                int ay = a[1] + v.getHeight() / 2;
                int by = b[1] + t.getHeight() / 2;
                boolean dirOk = dir == android.view.View.FOCUS_DOWN ? (by >= ay) : (by <= ay);
                if (!dirOk) return true;
            }
            t.requestFocus();
            return true;
        };
        for (android.widget.RadioGroup g : new android.widget.RadioGroup[]{rg, rgMode, rgIdle}) {
            for (int i = 0; i < g.getChildCount(); i++) g.getChildAt(i).setOnKeyListener(dirNav);
        }
        rowFill.setOnKeyListener(dirNav);
        swFill.setOnKeyListener(dirNav);
        btnScan.setOnKeyListener(dirNav);
        bCancelV.setOnKeyListener(dirNav);
        bSaveV.setOnKeyListener(dirNav);
    }

    /** 局域网扫描服务端：本机网段 1-254 并发探测 /api/stats，端口可指定（默认 8086）。 */
    private void scanServers(int port, Callback<List<String>> done) {
        exec.execute(() -> {
            final List<String> found = Collections.synchronizedList(new ArrayList<>());
            List<String> subnets = localSubnets();
            if (subnets.isEmpty()) {
                subnets.add("192.168.1.");
                subnets.add("192.168.0.");
            }
            ExecutorService pool = Executors.newFixedThreadPool(32);
            for (String sub : subnets) {
                for (int i = 1; i <= 254; i++) {
                    final String ip = sub + i;
                    pool.execute(() -> {
                        try {
                            HttpURLConnection c = (HttpURLConnection) new URL("http://" + ip + ":" + port + "/api/stats").openConnection();
                            c.setConnectTimeout(700);
                            c.setReadTimeout(700);
                            if (c.getResponseCode() == 200) {
                                found.add(ip + ":" + port);
                            }
                            c.disconnect();
                        } catch (Exception ignored) {
                        }
                    });
                }
            }
            pool.shutdown();
            try {
                pool.awaitTermination(10, TimeUnit.SECONDS);
            } catch (InterruptedException ignored) {
            }
            final List<String> result = new ArrayList<>(found);
            Collections.sort(result);
            ui.post(() -> done.call(result));
        });
    }

    /** 本机 IPv4 网段列表（去掉最后一段）。 */
    private List<String> localSubnets() {
        List<String> out = new ArrayList<>();
        try {
            Enumeration<NetworkInterface> nis = NetworkInterface.getNetworkInterfaces();
            while (nis.hasMoreElements()) {
                NetworkInterface ni = nis.nextElement();
                if (!ni.isUp() || ni.isLoopback()) continue;
                Enumeration<InetAddress> addrs = ni.getInetAddresses();
                while (addrs.hasMoreElements()) {
                    InetAddress a = addrs.nextElement();
                    if (a.isLoopbackAddress()) continue;
                    byte[] b = a.getAddress();
                    if (b.length == 4) {
                        out.add((b[0] & 0xFF) + "." + (b[1] & 0xFF) + "." + (b[2] & 0xFF) + ".");
                    }
                }
            }
        } catch (Exception ignored) {
        }
        return out;
    }

    private void restartPlayback() {
        playingSongId = -1;
        hlsFallback = false;
        cancelDirectWatchdog();
        if (core != null) {
            core.stop();
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
        Log.d(TAG, "onResume: corePlaying=" + (core != null && core.isPlaying()) + " userPaused=" + userPaused);
        if (core != null && core.isPlaying()) core.resume();
        // 唤醒回前台时若 HLS 仍卡缓冲（未暂停且未真正起播）→ 重启看门狗兜底，避免一直转圈
        if (core != null && !core.isPlaying() && !userPaused && playingSongId > 0 && !hlsEverPlayed
                && ("hls".equals(playMode()) || hlsFallback)) {
            startHlsWatchdog();
        }
    }

    @Override
    protected void onPause() {
        super.onPause();
        Log.d(TAG, "onPause: corePlaying=" + (core != null && core.isPlaying()) + " userPaused=" + userPaused);
        if (core != null && core.isPlaying()) core.pause();
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        ui.removeCallbacksAndMessages(null);
        cancelDirectWatchdog();
        cancelHlsWatchdog();
        if (ctrlWS != null) ctrlWS.close();
        if (core != null) {
            core.release();
            core = null;
        }
        exec.shutdownNow();
    }

    /** 简单回调（扫描服务端结果）。 */
    public interface Callback<T> {
        void call(T value);
    }
}
