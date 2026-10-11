plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Sailnet Lite: one button and an access code. The code is checked by the
// gate (lite-gate/), which pairs this phone with one relay; the phone then
// rides that relay directly, free, until the code runs out. Same Go client
// as the full app (app/libs/sail.aar), none of its settings.
fun git(vararg args: String): String = try {
    val p = ProcessBuilder("git", *args).directory(rootDir).redirectErrorStream(true).start()
    p.inputStream.bufferedReader().readText().trim().also { p.waitFor() }
} catch (e: Exception) { "" }
val gitDescribe = git("describe", "--tags", "--always").removePrefix("v").ifEmpty { "0.0.0" }
val gitCount = git("rev-list", "--count", "HEAD").toIntOrNull() ?: 1

android {
    namespace = "net.sailnet.lite"
    compileSdk = 34
    buildFeatures { buildConfig = true }

    defaultConfig {
        applicationId = "net.sailnet.lite"
        minSdk = 24
        targetSdk = 34
        versionCode = gitCount
        versionName = gitDescribe
        buildConfigField("String", "GATE", "\"${System.getenv("SAIL_LITE_GATE") ?: "https://sailnet-lite-gate.vercel.app"}\"")
    }
    signingConfigs {
        // Release signing from the environment, as in :app; the debug key locally.
        create("release") {
            val ks = System.getenv("SAIL_KEYSTORE")
            if (!ks.isNullOrEmpty()) {
                storeFile = file(ks)
                storePassword = System.getenv("SAIL_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("SAIL_KEY_ALIAS")
                keyPassword = System.getenv("SAIL_KEY_PASSWORD")
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
            include("arm64-v8a", "x86_64") // what sail.aar is built for
            isUniversalApk = true
        }
    }
}

dependencies {
    implementation(files("../app/libs/sail.aar"))
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("com.google.android.material:material:1.12.0")
}
