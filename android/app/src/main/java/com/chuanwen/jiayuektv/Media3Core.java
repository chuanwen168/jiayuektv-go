package com.chuanwen.jiayuektv;

import android.content.Context;
import android.media.audiofx.Equalizer;
import android.os.Handler;
import android.os.Looper;

import androidx.annotation.OptIn;
import androidx.media3.common.MediaItem;
import androidx.media3.common.PlaybackException;
import androidx.media3.common.Player;
import androidx.media3.exoplayer.ExoPlayer;
import androidx.media3.ui.PlayerView;

/**
 * Media3(ExoPlayer) 播放内核 —— HLS 默认内核（直连也可用）。
 *
 * 对齐精简版"播放正常"的成熟方案：
 *   - 切音轨走自定义 Media3TrackSelector.setForcedAudioTrack()+invalidate()：
 *     只重选音频轨、不重载播放器、不重启渲染管线，原位无缝切换不卡顿；
 *   - 服务端 HLS 为渐进式 event playlist（转码完成前没有 ENDLIST），ExoPlayer
 *     会按 live 流从 "live edge（末尾）" 起播 → 表现为首播从中间开始。设置超大
 *     targetOffsetMs（24h）强制从最老分片（=歌曲开头）起播，转码完成后
 *     （有 ENDLIST）自动回到 VOD 正常从头播放。
 */
@OptIn(markerClass = androidx.media3.common.util.UnstableApi.class)
public class Media3Core implements PlayerCore {

    private final Context context;
    private ExoPlayer player;
    private Media3TrackSelector trackSelector;
    private PlayerView view;
    private Listener listener;
    private final Handler ui = new Handler(Looper.getMainLooper());
    private long pendingSeek = -1; // 就绪后需要 seek 到的位置（直连切音轨恢复进度）
    private volatile boolean released = false; // 已释放：所有异步回调/延迟任务失效
    // 均衡器（audiofx 绑定 ExoPlayer audio session；切歌同一实例 session 不变，持续生效）
    private Equalizer audioEq;
    private int audioEqSession = -1;
    private float[] pendingEqGains; // player 未就绪时暂存，READY 后应用

    public Media3Core(Context context) {
        this.context = context.getApplicationContext();
    }

    private ExoPlayer player() {
        if (player == null) {
            trackSelector = new Media3TrackSelector(context);
            player = new ExoPlayer.Builder(context).setTrackSelector(trackSelector).build();
            player.setPlayWhenReady(true);
            player.addListener(new Player.Listener() {
                @Override
                public void onPlaybackStateChanged(int state) {
                    if (released) return; // 内核已释放：忽略迟到事件
                    if (state == Player.STATE_READY) {
                        if (pendingSeek >= 0) {
                            player.seekTo(pendingSeek);
                            pendingSeek = -1;
                        }
                        // 均衡器：session 此时才有效，补应用未就绪时暂存的设置
                        if (pendingEqGains != null) {
                            int sid = player.getAudioSessionId();
                            if (sid > 0) {
                                applyEq(pendingEqGains, sid);
                                pendingEqGains = null;
                            }
                        }
                    } else if (state == Player.STATE_ENDED && listener != null) {
                        listener.onEnded();
                    }
                }

                @Override
                public void onPlayerError(PlaybackException error) {
                    if (released) return;
                    if (listener != null) {
                        listener.onError(error != null ? error.getErrorCodeName() : "播放失败");
                    }
                }
            });
        }
        return player;
    }

    @Override
    public void attach(Object surface) {
        if (surface instanceof PlayerView) {
            view = (PlayerView) surface;
            view.setPlayer(player());
        }
    }

    @Override
    public void play(String url) {
        MediaItem item = new MediaItem.Builder()
                .setUri(url)
                .setLiveConfiguration(new MediaItem.LiveConfiguration.Builder()
                        .setTargetOffsetMs(24L * 60 * 60 * 1000)
                        .build())
                .build();
        player().setMediaItem(item, 0);
        player().prepare();
        player().play();
    }

    @Override
    public void pause() {
        if (player != null) player.pause();
    }

    @Override
    public void resume() {
        if (player != null) player.play();
    }

    @Override
    public void seekTo(long ms) {
        if (player != null) player.seekTo(ms);
    }

    @Override
    public void seekAfterReady(long ms) {
        pendingSeek = Math.max(0, ms);
        // 已就绪则立即 seek（避免等待下一次状态回调）
        if (player != null && player.getPlaybackState() == Player.STATE_READY) {
            player.seekTo(pendingSeek);
            pendingSeek = -1;
        }
    }

    @Override
    public long getPosition() {
        return player != null ? player.getCurrentPosition() : 0;
    }

    @Override
    public void setVolume(float v) {
        if (player != null) player.setVolume(Math.max(0f, Math.min(1f, v)));
    }

    @Override
    public float getVolume() {
        return player != null ? player.getVolume() : 1f;
    }

    @Override
    public boolean isPlaying() {
        return player != null && player.isPlaying();
    }

    @Override
    public void stop() {
        if (player != null) {
            player.stop();
            player.clearMediaItems();
        }
    }

    @Override
    public void release() {
        released = true;       // 先置标志：事件回调/延迟任务全部失效
        listener = null;       // 解绑监听：release 触发的异步事件不再回调上层
        releaseEq();
        ui.removeCallbacksAndMessages(null);
        if (view != null) {
            view.setPlayer(null);
            view = null;
        }
        if (player != null) {
            player.release();
            player = null;
        }
    }

    @Override
    public void setListener(Listener l) {
        listener = l;
    }

    /**
     * HLS 多音轨原位切换：自定义 TrackSelector 按音频组索引强制选择并 invalidate()。
     * 播放前/播放中调用均安全（选择器持久保存强制索引，首次选择或重选时生效）。
     */
    @Override
    public void setAudioTrack(int trackIndex) {
        if (trackSelector != null) {
            trackSelector.setForcedAudioTrack(trackIndex);
        }
    }

    /**
     * 应用 10 段均衡器（控制端 60Hz~16kHz，-12~+12dB）：
     * ExoPlayer 无均衡器 API，用系统 audiofx.Equalizer 绑定其 audio session。
     * Android 均衡器固定 5 段（63/250/1k/4k/16k），10 段按频率就近映射；
     * 全 0 或 null 时旁路；session 未就绪时暂存，READY 后自动应用。
     */
    @Override
    public void setEqualizer(float[] gains) {
        if (gains == null || gains.length == 0) {
            pendingEqGains = null;
            releaseEq();
            return;
        }
        ExoPlayer p = player;
        if (p == null) {
            pendingEqGains = gains;
            return;
        }
        int sid = p.getAudioSessionId();
        if (sid <= 0) {
            pendingEqGains = gains; // 未就绪，等 READY 补应用
            return;
        }
        applyEq(gains, sid);
        pendingEqGains = null;
    }

    private void applyEq(float[] gains, int sid) {
        try {
            boolean allZero = true;
            for (float g : gains) if (g != 0) { allZero = false; break; }
            if (allZero) {
                releaseEq();
                return;
            }
            if (audioEq == null || audioEqSession != sid) {
                releaseEq();
                audioEq = new Equalizer(0, sid);
                audioEqSession = sid;
            }
            // 10 段 → 系统 5 段映射（63/250/1k/4k/16k），dB→millibel
            short[] levels = new short[]{
                    clampDb(gains[0]),                    // 63Hz  ← 60Hz
                    clampDb((gains[1] + gains[2]) / 2f),  // 250Hz ← 170/310
                    clampDb(gains[4]),                    // 1kHz  ← 1000Hz
                    clampDb((gains[5] + gains[6]) / 2f),  // 4kHz  ← 3k/6k
                    clampDb(gains[9])                     // 16kHz ← 16000Hz
            };
            int bands = audioEq.getNumberOfBands();
            for (int i = 0; i < bands && i < levels.length; i++) {
                audioEq.setBandLevel((short) i, levels[i]);
            }
            audioEq.setEnabled(true);
        } catch (Exception e) {
            // 设备不支持/权限受限：静默降级（不影响播放）
            releaseEq();
        }
    }

    private short clampDb(float db) {
        return (short) Math.max(-1200, Math.min(1200, Math.round(db * 100)));
    }

    private void releaseEq() {
        if (audioEq != null) {
            try {
                audioEq.setEnabled(false);
                audioEq.release();
            } catch (Exception ignored) {
            }
            audioEq = null;
            audioEqSession = -1;
        }
    }

    @Override
    public void setFillScreen(boolean fill) {
        // Media3 由 PlayerView.setResizeMode 控制（MainActivity.applyFillScreen 处理）
    }
}
