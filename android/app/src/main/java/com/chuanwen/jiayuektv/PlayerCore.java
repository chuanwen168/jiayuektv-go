package com.chuanwen.jiayuektv;

/**
 * 播放内核抽象：屏蔽 Media3(ExoPlayer) / VLC(libvlc) 实现差异，
 * 统一给 MainActivity 提供播放控制。
 *
 * 职责边界（设置页规则）：
 *   - HLS 模式固定使用 Media3（支持多音轨原位无缝切换，不重载播放器）；
 *   - 直连模式默认使用 VLC（全格式硬解，失败自动软解），可切换 Media3；
 *   - 直连播放失败/不支持时，由 MainActivity 自动回退到 HLS(Media3)。
 */
public interface PlayerCore {

    /** 内核事件回调（主线程）。 */
    interface Listener {
        /** 播放结束（歌曲自然播完）。 */
        void onEnded();

        /** 播放失败（解码失败 / 网络错误 / 内核不可用）。message 为诊断信息。 */
        void onError(String message);
    }

    /** 绑定显示容器：Media3 用 PlayerView，VLC 用 VLCVideoLayout。 */
    void attach(Object surface);

    /** 播放指定 URL（从头开始）。url 可为 HLS master 或直连 /stream。 */
    void play(String url);

    void pause();

    void resume();

    void seekTo(long ms);

    /** 播放/重载后，等内核就绪（可 seek）再跳到指定位置（直连切音轨续播用）。 */
    void seekAfterReady(long ms);

    long getPosition();

    void setVolume(float v);

    float getVolume();

    boolean isPlaying();

    void stop();

    void release();

    void setListener(Listener l);

    /** 按音轨索引切换（0=原唱 1=伴唱）。HLS 走 TrackSelector 原位切换；VLC 走原生音轨。 */
    void setAudioTrack(int trackIndex);

    /**
     * 应用 10 段均衡器增益（dB，-12 ~ +12，对应控制端 60Hz~16kHz 十段）。
     * gains 为 null 或全 0 时清除/旁路均衡器。
     */
    void setEqualizer(float[] gains);

    /** 播放画面撑满屏幕：true=等比放大裁剪填满（HLS 的 ZOOM 效果），false=按比例适应。 */
    void setFillScreen(boolean fill);
}
