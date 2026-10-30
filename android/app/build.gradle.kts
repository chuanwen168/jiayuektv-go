plugins {
    alias(libs.plugins.android.application)
}

android {
    namespace = "com.chuanwen.jiayuektv"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.chuanwen.jiayuektv"
        minSdk = 23
        targetSdk = 35
        versionCode = 4
        versionName = "1.0.5" //

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    // 按 ABI 拆分两个版本：armv7 / arm64
    flavorDimensions += "abi"
    productFlavors {
        create("armv7") {
            dimension = "abi"
            ndk { abiFilters += listOf("armeabi-v7a") }
        }
        create("arm64") {
            dimension = "abi"
            ndk { abiFilters += listOf("arm64-v8a") }
        }
        // 临时加入 x86：仅调试用（x86 模拟器/调试环境），正式发布不打 x86 包
        create("x86") {
            dimension = "abi"
            ndk { abiFilters += listOf("x86") }
        }
    }


    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            signingConfig = signingConfigs.getByName("release")
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_11
        targetCompatibility = JavaVersion.VERSION_11
    }
}

dependencies {

    implementation(libs.appcompat)
    implementation(libs.material)
    implementation(libs.activity)
    implementation(libs.constraintlayout)
        // 播放内核（HLS + 音轨切换，Android 原生解码器直解）
    implementation("androidx.media3:media3-exoplayer:1.4.1")
    implementation("androidx.media3:media3-exoplayer-hls:1.4.1")
    implementation("androidx.media3:media3-ui:1.4.1")
    // VLC 直连内核（全格式直解，AV1/H265/MKV 硬解失败自动软解；可选内核，设置页切换）
    implementation("org.videolan.android:libvlc-all:3.7.0")
    // 二维码生成（扫码点歌）
    implementation("com.google.zxing:core:3.5.3")
    // WebSocket 控制通道（接收手机/平板端 control 消息）
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    testImplementation(libs.junit)
    androidTestImplementation(libs.ext.junit)
    androidTestImplementation(libs.espresso.core)
}