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
        // 外观对齐网页版：默认浅白 rgba(240,242,255,.8)、18px、字重500、深色低透底
        setTextColor(Color.argb(204, 240, 242, 255));
        setTextSize(18);
        setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        setBackgroundColor(Color.argb(64, 8, 6, 20));
        setLetterSpacing(0.03f);
        setPadding(dp(12), dp(4), dp(12), dp(4));
        setGravity(Gravity.CENTER_VERTICAL);
        setVisibility(GONE);
    }

    private int dp(int v) {
        return Math.round(getResources().getDisplayMetrics().density * v);
    }

    /** 开始滚动一次：从父容器右边缘外滚到完全移出左侧，结束后隐藏。 */
    public void startTicker(CharSequence text) {
        stop();
        setText(text);
        setVisibility(VISIBLE);
        measure(0, 0);
        // 父容器宽度（从父布局取）
        View parent = (View) getParent();
        int parentW = parent != null ? parent.getWidth() : 1920;
        int textW = getMeasuredWidth();
        float startX = parentW + 20;          // 屏幕最右之外
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
