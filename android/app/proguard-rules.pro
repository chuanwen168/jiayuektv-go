# Add project specific ProGuard rules here.
# You can control the set of applied configuration files using the
# proguardFiles setting in build.gradle.
#
# For more details, see
#   http://developer.android.com/guide/developing/tools/proguard.html

# If your project uses WebView with JS, uncomment the following
# and specify the fully qualified class name to the JavaScript interface
# class:
#-keepclassmembers class fqcn.of.javascript.interface.for.webview {
#   public *;
#}

# Uncomment this to preserve the line number information for
# debugging stack traces.
#-keepattributes SourceFile,LineNumberTable

# If you keep the line number information, uncomment this to
# hide the original source file name.
#-renamesourcefileattribute SourceFile

# ===== VLC 直连内核 =====
# libvlc-all 3.7.0 的 AAR 不自带 consumer rules，而 libvlc 通过 JNI 反射引用这些
# 接口类（IMedia$Track 等）。不加 keep 规则时 R8 会把它们当死代码删除/改名，
# 导致 JNI_OnLoad 返回 JNI_ERR、创建 VLCCore 时 UnsatisfiedLinkError 闪退。
-keep class org.videolan.libvlc.** { *; }
-keep class org.videolan.medialibrary.** { *; }