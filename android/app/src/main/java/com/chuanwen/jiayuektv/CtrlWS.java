package com.chuanwen.jiayuektv;

import android.os.Handler;
import android.os.Looper;

import org.json.JSONObject;

import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

import okhttp3.OkHttpClient;
import okhttp3.Request;
import okhttp3.Response;
import okhttp3.WebSocket;
import okhttp3.WebSocketListener;

/**
 * 控制通道 WebSocket：连接服务端 /ws，接收手机/平板点歌端发来的 control 消息，
 * 与网页 TV 端走同一通道（服务端把 control 消息广播给所有播放端）。
 */
public class CtrlWS {

    public interface Listener {
        /** 收到 control 消息（action 已解析，msg 为原始消息）。 */
        void onControl(String action, JSONObject msg);

        /** 收到氛围互动消息（effect=欢呼/鼓掌/尖叫/口哨/喝倒彩/再来一首，nickname=点歌人昵称）。 */
        default void onEffect(String effect, String nickname) {
        }

        /** 连接状态变化（connected=true 表示已连接服务端）。 */
        default void onStatus(boolean connected) {
        }
    }

    private static final long RECONNECT_DELAY_MS = 3000;

    private final OkHttpClient client = new OkHttpClient();
    private final Handler ui = new Handler(Looper.getMainLooper());
    private final ExecutorService exec = Executors.newSingleThreadExecutor();
    private final Listener listener;

    private volatile WebSocket socket;
    private volatile boolean stopped = false;
    private volatile boolean connected = false;

    public CtrlWS(Listener listener) {
        this.listener = listener;
    }

    /** 连接（或重连）到 Api.base 的 /ws。 */
    public void connect() {
        stopped = false;
        exec.execute(this::doConnect);
    }

    private void doConnect() {
        String base = Api.base;
        if (stopped || base.isEmpty()) return;
        try {
            String wsUrl = base.replaceFirst("^http", "ws") + "/ws";
            Request req = new Request.Builder().url(wsUrl).build();
            socket = client.newWebSocket(req, new WebSocketListener() {
                @Override
                public void onOpen(WebSocket webSocket, Response response) {
                    if (stopped) return;
                    connected = true;
                    ui.post(() -> listener.onStatus(true));
                }

                @Override
                public void onMessage(WebSocket webSocket, String text) {
                    try {
                        JSONObject msg = new JSONObject(text);
                        if ("control".equals(msg.optString("type"))) {
                            final String action = msg.optString("action", "");
                            if (!action.isEmpty()) {
                                ui.post(() -> listener.onControl(action, msg));
                            }
                        } else if ("effect".equals(msg.optString("type"))) {
                            final String effect = msg.optString("effect", "");
                            if (!effect.isEmpty()) {
                                final String nick = msg.optString("nickname", "");
                                ui.post(() -> listener.onEffect(effect, nick));
                            }
                        }
                    } catch (Exception ignored) {
                    }
                }

                @Override
                public void onClosed(WebSocket webSocket, int code, String reason) {
                    scheduleReconnect();
                }

                @Override
                public void onFailure(WebSocket webSocket, Throwable t, Response response) {
                    scheduleReconnect();
                }
            });
        } catch (Exception e) {
            scheduleReconnect();
        }
    }

    private void scheduleReconnect() {
        if (stopped) return;
        if (connected) {
            connected = false;
            ui.post(() -> listener.onStatus(false));
        }
        ui.postDelayed(() -> {
            if (!stopped) connect();
        }, RECONNECT_DELAY_MS);
    }

    /** 停止并关闭连接（退出或更换服务器时）。 */
    public void close() {
        stopped = true;
        ui.removeCallbacksAndMessages(null);
        exec.execute(() -> {
            WebSocket s = socket;
            socket = null;
            if (s != null) {
                try {
                    s.close(1000, "bye");
                } catch (Exception ignored) {
                }
            }
        });
    }
}
