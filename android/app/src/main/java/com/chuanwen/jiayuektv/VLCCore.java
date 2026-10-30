package com.chuanwen.jiayuektv;

import android.content.Context;
import android.net.Uri;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import org.videolan.libvlc.LibVLC;
import org.videolan.libvlc.Media;
import org.videolan.libvlc.MediaPlayer;
import org.videolan.libvlc.util.VLCVideoLayout;

/**
 * VLC(libvlc) 播放内核 —— 直连模式默认内核。
 * 全格式直解（AV1/H265/MKV 等 Media3 直连解不了的格式），硬解优先失败自动软解；
 * 直连 fMP4 渐进式流在部分老盒子（晶晨 S905 等）上硬解渲染会白屏 → 显式软解兜底。
 */
public class VLCCore implements PlayerCore {

    private final LibVLC libVLC;
    private final MediaPlayer mediaPlayer;
    private final Handler ui = new Handler(Looper.getMainLooper());
    private VLCVideoLayout layout;
    private Listener listener;
    private float volume = 1f;
    private long pendingSeek = -1; // 就绪后需要 seek 到的位置
    private int pendingTrack = -1; // 音轨枚举就绪后需要应用的目标音轨
    private String currentUrl;     // 当前播放的 URL（resume 重载兜底用）
    private MediaPlayer.Equalizer currentEq; // 当前均衡器（保存引用防 GC 释放）
    private volatile boolean released = false; // 已释放：所有异步回调/延迟任务失效
    private final Runnable seekPoller = new Runnable() {
        @Override
        public void run() {
            if (pendingSeek < 0) return;
            try {
                if (mediaPlayer.isPlaying() && mediaPlayer.getTime() >= 0) {
                    mediaPlayer.setTime(pendingSeek);
                    pendingSeek = -1;
                } else {
                    ui.postDelayed(this, 250);
                }
            } catch (Exception e) {
                ui.postDelayed(this, 250);
            }
        }
    };
    private final Runnable trackPoller = new Runnable() {
        @Override
        public void run() {
            if (pendingTrack < 0) return;
            if (applyTrackNow(pendingTrack)) {
                pendingTrack = -1;
            } else {
                ui.postDelayed(this, 200); // 轨道未枚举完，稍后重试
            }
        }
    };

    public VLCCore(Context context) {
        libVLC = new LibVLC(context.getApplicationContext());
        mediaPlayer = new MediaPlayer(libVLC);
        mediaPlayer.setVolume((int) (volume * 100));
        mediaPlayer.setEventListener(new MediaPlayer.EventListener() {
            @Override
            public void onEvent(MediaPlayer.Event event) {
                if (released) return; // 内核已释放：忽略迟到事件（防干扰新内核）
                if (event.type == MediaPlayer.Event.EndReached) {
                    if (listener != null) listener.onEnded();
                } else if (event.type == MediaPlayer.Event.EncounteredError) {
                    if (listener != null) listener.onError("VLC 播放失败");
                }
            }
        });
    }

    @Override
    public void attach(Object surface) {
        if (surface instanceof VLCVideoLayout) {
            layout = (VLCVideoLayout) surface;
            // TextureView 渲染：兼容部分老盒子（晶晨 S905 等）SurfaceView 合成白屏的问题。
            mediaPlayer.attachViews(layout, null, true, false);
        }
    }

    @Override
    public void play(String url) {
        currentUrl = url;
        Media media = new Media(libVLC, Uri.parse(url));
        if (pendingTrack >= 0) {
            // 起播前预选目标音轨（libVLC 音轨 id 从 1 起）：避免起播默认音轨后再切换
            // 导致音频管线重建、起播几秒无声
            media.addOption(":audio-track=" + (pendingTrack + 1));
        }
        // 硬解优先，失败自动回退软解（现代盒子/电视硬解正常；老盒子硬解失败时 VLC 自动软解）
        media.setHWDecoderEnabled(true, false);
        mediaPlayer.play(media);
        media.release();
    }

    @Override
    public void pause() {
        // libvlc 的 pause() 是「切换」语义：仅在播放中调用，避免已暂停时再次调用变成恢复播放
        if (mediaPlayer.isPlaying()) {
            mediaPlayer.pause();
        }
    }

    @Override
    public void resume() {
        try {
            if (mediaPlayer.isPlaying()) return; // 已在播放
            final long pos = mediaPlayer.getTime(); // 暂停位置（重载兜底 seek 用）
            int st = mediaPlayer.getPlayerState();
            Log.e("VLCCore", "resume: state=" + st + " pos=" + pos);
            if (st == 4) {
                // libvlc 3.x 的 pause() 是切换语义（paused 时调用恢复）；
                // libvlc 4.x 的 pause() 是单向暂停（此调用无效，走下方重载兜底）。
                mediaPlayer.pause();
            } else if (st == 1 || st == 2 || st == 3) {
                // Opening/Buffering/Playing：缓冲中或正在播放，非暂停状态，不做任何操作，
                // 尤其不触发重载兜底（避免"起播缓冲期点暂停"被误判为恢复而重载流）。
                return;
            } else {
                // 停止/结束/错误等状态：尝试重新启动
                mediaPlayer.play();
            }
            // 重载兜底（仅暂停状态走这里）：400ms 后仍未恢复播放 → 重新加载当前流 +
            // seek 回暂停位置，不依赖 libvlc 的恢复语义，保证"暂停后点播放"一定续播不从头。
            if (st == 4) {
                final String url = currentUrl;
                final long guardPos = pos;
                ui.postDelayed(() -> {
                    try {
                        if (mediaPlayer.isPlaying()) return;
                        Log.e("VLCCore", "resume: native failed, reload url=" + url
                                + " state=" + mediaPlayer.getPlayerState());
                        reloadCurrent();
                        if (guardPos > 500) {
                            mediaPlayer.setTime(guardPos);
                            Log.e("VLCCore", "resume: seek back to " + guardPos);
                        }
                    } catch (Exception ignored) {
                    }
                }, 400);
            }
        } catch (Exception e) {
            Log.e("VLCCore", "resume err", e);
        }
    }

    /** 重新加载当前流（resume 兜底用；硬解优先软解兜底）。 */
    private void reloadCurrent() {
        if (currentUrl == null) return;
        try {
            Media media = new Media(libVLC, Uri.parse(currentUrl));
            if (pendingTrack >= 0) {
                media.addOption(":audio-track=" + (pendingTrack + 1));
            }
            media.setHWDecoderEnabled(true, false);
            mediaPlayer.play(media);
            media.release();
        } catch (Exception ignored) {
        }
    }

    @Override
    public void seekTo(long ms) {
        mediaPlayer.setTime(ms);
    }

    @Override
    public void seekAfterReady(long ms) {
        pendingSeek = Math.max(0, ms);
        ui.removeCallbacks(seekPoller);
        ui.postDelayed(seekPoller, 250);
    }

    @Override
    public long getPosition() {
        return mediaPlayer.getTime();
    }

    @Override
    public void setVolume(float v) {
        volume = Math.max(0f, Math.min(1f, v));
        mediaPlayer.setVolume((int) (volume * 100));
    }

    @Override
    public float getVolume() {
        return volume;
    }

    @Override
    public boolean isPlaying() {
        return mediaPlayer.isPlaying();
    }

    @Override
    public void stop() {
        mediaPlayer.stop();
    }

    @Override
    public void release() {
        released = true;       // 先置标志：事件回调/延迟任务全部失效
        listener = null;       // 解绑监听：release 触发的异步事件不再回调上层
        pendingSeek = -1;
        pendingTrack = -1;
        ui.removeCallbacksAndMessages(null); // 清所有延迟任务（含 resume 兜底 lambda）
        if (layout != null) {
            mediaPlayer.detachViews();
            layout = null;
        }
        mediaPlayer.release();
        libVLC.release();
    }

    @Override
    public void setListener(Listener l) {
        listener = l;
    }

    /**
     * 直连模式音轨切换：VLC 原生切换源文件的双音轨（原唱/伴唱），服务端不转码。
     * 刚起播时音轨可能尚未枚举，先按名字匹配「原唱/audio0」「伴唱/audio1」，
     * 匹配不到按 VLC 音轨 id 兜底（id 从 1 起，文件音频流顺序 0=原唱、1=伴唱）；
     * 枚举未完成时每 200ms 重试，最多约 15s。
     */
    @Override
    public void setAudioTrack(int trackIndex) {
        pendingTrack = trackIndex;
        ui.removeCallbacks(trackPoller);
        if (applyTrackNow(trackIndex)) {
            pendingTrack = -1;
        } else {
            ui.postDelayed(trackPoller, 200);
        }
    }

    private boolean applyTrackNow(int trackIndex) {
        try {
            MediaPlayer.TrackDescription[] tracks = mediaPlayer.getAudioTracks();
            if (tracks == null || tracks.length == 0) return false; // 尚未枚举
            int id = -1;
            String want1 = trackIndex == 1 ? "伴唱" : "原唱";
            String want2 = trackIndex == 1 ? "audio1" : "audio0";
            for (MediaPlayer.TrackDescription t : tracks) {
                String name = t.name == null ? "" : t.name;
                if (name.contains(want1) || name.contains(want2)) {
                    id = t.id;
                    break;
                }
            }
            if (id <= 0) id = trackIndex + 1; // 兜底：VLC 音轨 id 从 1 开始
            mediaPlayer.setAudioTrack(id);
            return true;
        } catch (Exception e) {
            return false;
        }
    }

    /**
     * 应用 10 段均衡器（控制端 60Hz~16kHz 十段增益，-12~+12dB）：
     * 用 libvlc 原生 Equalizer（10 段），控制端频率与 libvlc 频段
     * （约 32/64/125/250/500/1k/2k/4k/8k/16k）按"频率就近"映射而非按索引硬映射，
     * 保证低频/中频/高频增益落到正确频段、效果可闻。
     * 均衡器 audiofilter 有固定衰减（听感音量变小）→ 用 preamp 预放大补偿。
     * 全 0 或 null 时旁路。同一 mediaPlayer 实例上持续生效，切歌不丢失。
     */
    @Override
    public void setEqualizer(float[] gains) {
        try {
            if (gains == null || gains.length == 0 || allZero(gains)) {
                mediaPlayer.setEqualizer(null);
                currentEq = null;
                return;
            }
            MediaPlayer.Equalizer eq = MediaPlayer.Equalizer.create();
            int n = eq.getBandCount();
            // 控制端 10 段频率（与 web 端 EQ_FREQS 一致）
            float[] ctlFreq = {60, 170, 310, 600, 1000, 3000, 6000, 12000, 14000, 16000};
            for (int gi = 0; gi < Math.min(gains.length, 10); gi++) {
                int best = 0;
                float bestD = Float.MAX_VALUE;
                for (int i = 0; i < n; i++) {
                    float d = Math.abs(eq.getBandFrequency(i) - ctlFreq[gi]);
                    if (d < bestD) {
                        bestD = d;
                        best = i;
                    }
                }
                float g = Math.max(-12f, Math.min(12f, gains[gi]));
                eq.setAmp(best, g);
            }
            // filter 固定衰减补偿：预放大 6dB（libvlc clamp -20~+20，安全）
            eq.setPreAmp(6f);
            mediaPlayer.setEqualizer(eq);
            currentEq = eq;
        } catch (Exception e) {
            Log.e("VLCCore", "setEqualizer err", e);
        }
    }

    private boolean allZero(float[] a) {
        for (float v : a) if (v != 0) return false;
        return true;
    }

    @Override
    public void setFillScreen(boolean fill) {
        try {
            // 原生视频缩放：SURFACE_FILL=等比放大裁剪填满（同 HLS ZOOM），BEST_FIT=按比例适应
            mediaPlayer.setVideoScale(fill
                    ? MediaPlayer.ScaleType.SURFACE_FILL
                    : MediaPlayer.ScaleType.SURFACE_BEST_FIT);
        } catch (Exception e) {
            Log.e("VLCCore", "setFillScreen err", e);
        }
    }
}
