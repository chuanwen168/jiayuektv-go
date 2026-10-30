package com.chuanwen.jiayuektv;

import android.animation.Animator;
import android.animation.AnimatorListenerAdapter;
import android.animation.ObjectAnimator;
import android.animation.ValueAnimator;
import android.content.Context;
import android.graphics.Color;
import android.graphics.Typeface;
import android.util.AttributeSet;
import android.view.Gravity;
import android.view.View;
import android.widget.TextView;

import androidx.appcompat.widget.AppCompatTextView;

/**
 * TV 顶部滚动字幕：间歇出现，从屏幕最右滚动到最左，跑完一次即消失。
 * 外观对齐网页版：顶部浮层、深色低透底、浅色文字、18px 大小。
 */
public class TickerView extends AppCompatTextView {

    /** 单次滚动时长（毫秒），对应网页版每段 25 秒。 */
    public static final long DURATION_MS = 25000;

    private ValueAnimator animator;
    private OnFinishedListener listener;

    public interface OnFinishedListener {
        void onFinished();
    }

    public TickerView(Context context) {
        this(context, null);
    }

    public TickerView(Context context, AttributeSet attrs) {
        super(context, attrs);
        setSingleLine(true);
        // 外观对齐网页版：浅白文字、大字号（24sp）、字重500、深色低透底
        setTextColor(Color.argb(230, 240, 242, 255));
        setTextSize(24);
        setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        setBackgroundColor(Color.argb(80, 8, 6, 20));
        setLetterSpacing(0.03f);
        setPadding(dp(16), dp(8), dp(16), dp(8));
        setGravity(Gravity.CENTER_VERTICAL);
        setVisibility(GONE);
    }

    private int dp(int v) {
        return Math.round(getResources().getDisplayMetrics().density * v);
    }

    /** 开始滚动一次：从父容器右边缘外滚到完全移出左侧，结束后隐藏。
     *  父容器 paddingEnd 用于避开右上角二维码（待机时常显），滚动只到 paddingEnd 左侧。 */
    public void startTicker(CharSequence text) {
        stop();
        setText(text);
        setVisibility(VISIBLE);
        measure(0, 0);
        // 父容器滚动宽度（减 paddingEnd 避开右上角二维码区域）
        View parent = (View) getParent();
        int parentW = parent != null ? parent.getWidth() - parent.getPaddingEnd() : 1920;
        int textW = getMeasuredWidth();
        float startX = parentW + 20;          // 有效区域最右之外
        float endX = -textW - 20;             // 完全移出最左
        setX(startX);
        animator = ObjectAnimator.ofFloat(this, View.X, startX, endX);
        animator.setDuration(DURATION_MS);
        animator.setInterpolator(null); // 线性匀速
        animator.addListener(new AnimatorListenerAdapter() {
            @Override
            public void onAnimationEnd(Animator a) {
                setVisibility(GONE);
                if (listener != null) listener.onFinished();
            }
        });
        animator.start();
    }

    public void setOnFinishedListener(OnFinishedListener l) {
        this.listener = l;
    }

    public void stop() {
        if (animator != null) {
            animator.cancel();
            animator = null;
        }
        setVisibility(GONE);
    }
}
