package com.chuanwen.jiayuektv;

import android.content.Context;
import android.util.Pair;

import androidx.annotation.OptIn;
import androidx.media3.common.C;
import androidx.media3.common.TrackGroup;
import androidx.media3.common.util.UnstableApi;
import androidx.media3.exoplayer.ExoPlaybackException;
import androidx.media3.exoplayer.source.TrackGroupArray;
import androidx.media3.exoplayer.trackselection.DefaultTrackSelector;
import androidx.media3.exoplayer.trackselection.ExoTrackSelection;

/**
 * 自定义音轨选择器（对齐精简版 KtvTrackSelector 的流畅方案）：
 * 按"音频组索引"强制选择音轨（0=原唱组, 1=伴唱组），setForcedAudioTrack 触发
 * invalidate() 让 TrackSelector 内部快速重选音频轨——不经过 Player 的
 * setTrackSelectionParameters 全链路（不会重选视频/重启渲染管线），
 * 从而像精简版一样原位流畅切换，避免卡顿。
 *
 * 兼容两种 HLS 结构：
 *   a) 分离组（每音轨一组）：第 0 个音频组=原唱，第 1 个=伴唱；
 *   b) 单组多音轨（一组 length>1）：组内索引 0=原唱、1=伴唱。
 */
@OptIn(markerClass = UnstableApi.class)
public class Media3TrackSelector extends DefaultTrackSelector {

    private volatile int forcedAudioTrack = -1; // -1=跟随默认逻辑, 0=原唱, 1=伴唱

    public Media3TrackSelector(Context context) {
        super(context);
    }

    /** 强制选择音轨并触发 TrackSelector 重选（不重载播放器/不碰视频轨）。 */
    public void setForcedAudioTrack(int track) {
        forcedAudioTrack = track;
        invalidate();
    }

    @Override
    @UnstableApi
    protected Pair<ExoTrackSelection.Definition, Integer> selectAudioTrack(
            MappedTrackInfo mappedTrackInfo, int[][][] rendererFormatSupports,
            int[] rendererMixedMimeTypeAdaptationSupports, Parameters params)
            throws ExoPlaybackException {
        int force = forcedAudioTrack;
        if (force >= 0) {
            for (int r = 0; r < mappedTrackInfo.getRendererCount(); r++) {
                if (mappedTrackInfo.getRendererType(r) != C.TRACK_TYPE_AUDIO) continue;
                TrackGroupArray groups = mappedTrackInfo.getTrackGroups(r);
                int audioIdx = 0;
                for (int g = 0; g < groups.length; g++) {
                    TrackGroup grp = groups.get(g);
                    if (grp.length > 0 && grp.getFormat(0).sampleMimeType != null
                            && grp.getFormat(0).sampleMimeType.startsWith("audio")) {
                        if (audioIdx == force) {
                            // 分离组：选中第 force 个音频组的 index 0；
                            // 单组多音轨：组内 index = force。
                            // 注意：Media3 的 selectAudioTrack 返回值 second 是 renderer 索引
                            //（DefaultTrackSelector.selectAllTracks 用它定位 renderer），
                            // 不是 formatSupport 支持级别。
                            int idx = (grp.length > 1 && force >= 1) ? force : 0;
                            return Pair.create(new ExoTrackSelection.Definition(grp, idx), r);
                        }
                        audioIdx++;
                    }
                }
            }
        }
        return super.selectAudioTrack(mappedTrackInfo, rendererFormatSupports,
                rendererMixedMimeTypeAdaptationSupports, params);
    }
}
