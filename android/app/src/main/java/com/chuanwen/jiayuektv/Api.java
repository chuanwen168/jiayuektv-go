package com.chuanwen.jiayuektv;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;

/** 家悦K歌服务器 HTTP 封装（同步方法，请在子线程调用）。 */
public class Api {

    /** 服务器根地址，如 http://192.168.1.100:8086，由设置页写入。 */
    public static volatile String base = "";

    public static class HttpResult {
        public int code;
        public String body;
    }

    /** GET 请求，返回原始文本。 */
    public static HttpResult get(String path) throws Exception {
        return request("GET", path, null);
    }

    /** POST 请求（JSON body 可选）。 */
    public static HttpResult post(String path, JSONObject body) throws Exception {
        return request("POST", path, body);
    }

    /** PUT 请求（JSON body 可选）。 */
    public static HttpResult put(String path, JSONObject body) throws Exception {
        return request("PUT", path, body);
    }

    /** DELETE 请求。 */
    public static HttpResult delete(String path) throws Exception {
        return request("DELETE", path, null);
    }

    private static HttpResult request(String method, String path, JSONObject body) throws Exception {
        URL u = new URL(base + path);
        HttpURLConnection c = (HttpURLConnection) u.openConnection();
        c.setRequestMethod(method);
        c.setConnectTimeout(5000);
        c.setReadTimeout(15000);
        c.setRequestProperty("Accept", "application/json");
        if (body != null) {
            c.setDoOutput(true);
            c.setRequestProperty("Content-Type", "application/json");
            byte[] b = body.toString().getBytes(StandardCharsets.UTF_8);
            try (OutputStream os = c.getOutputStream()) {
                os.write(b);
            }
        }
        int code = c.getResponseCode();
        InputStream is = code >= 400 ? c.getErrorStream() : c.getInputStream();
        StringBuilder sb = new StringBuilder();
        if (is != null) {
            try (BufferedReader r = new BufferedReader(new InputStreamReader(is, StandardCharsets.UTF_8))) {
                String line;
                while ((line = r.readLine()) != null) {
                    sb.append(line);
                }
            }
        }
        c.disconnect();
        HttpResult r = new HttpResult();
        r.code = code;
        r.body = sb.toString();
        return r;
    }
}
