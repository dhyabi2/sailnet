plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Version from git, so every tag is a new version to Android and to the
// user: versionName is the nearest tag (v0.2.9 -> 0.2.9, plus -N-gHASH when
// past it), versionCode the number of commits (always increasing).
fun git(vararg args: String): String = try {
    val p = ProcessBuilder("git", *args).directory(rootDir).redirectErrorStream(true).start()
    p.inputStream.bufferedReader().readText().trim().also { p.waitFor() }
} catch (e: Exception) { "" }
val gitDescribe = git("describe", "--tags", "--always").removePrefix("v").ifEmpty { "0.0.0" }
val gitCount = git("rev-list", "--count", "HEAD").toIntOrNull() ?: 1

android {
    namespace = "net.sailnet.app"
    compileSdk = 34
    buildFeatures { buildConfig = true }

    defaultConfig {
        applicationId = "net.sailnet.app"
        minSdk = 24
        targetSdk = 34
        versionCode = gitCount
        versionName = gitDescribe
    }
    signingConfigs {
        // Release signing from the environment (CI secrets); falls back to the debug key locally.
        create("release") {
            val ks = System.getenv("SAIL_KEYSTORE")
            if (!ks.isNullOrEmpty()) {
                storeFile = file(ks)
                storePassword = System.getenv("SAIL_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("SAIL_KEY_ALIAS")
                keyPassword = System.getenv("SAIL_KEY_PASSWORD")
                // PKCS12 by default: it is the portable format, and it can be
                // produced without a JDK on the machine that holds the key.
                storeType = System.getenv("SAIL_KEYSTORE_TYPE") ?: "PKCS12"
            }
        }
    }
    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = if (System.getenv("SAIL_KEYSTORE").isNullOrEmpty()) signingConfigs.getByName("debug") else signingConfigs.getByName("release")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions { jvmTarget = "17" }
    packaging { jniLibs.useLegacyPackaging = true }
    splits {
        abi {
            isEnable = true
            reset()
            include("arm64-v8a", "armeabi-v7a", "x86_64")
            isUniversalApk = true
        }
    }
}

dependencies {
    implementation(files("libs/sail.aar"))
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.preference:preference-ktx:1.2.1")
    implementation("com.google.android.material:material:1.12.0")
    implementation("com.google.zxing:core:3.5.3")
}
