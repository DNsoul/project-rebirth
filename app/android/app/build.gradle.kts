import java.util.Properties

plugins {
    id("com.android.application")
    id("kotlin-android")
    // The Flutter Gradle Plugin must be applied after the Android and Kotlin Gradle plugins.
    id("dev.flutter.flutter-gradle-plugin")
}

// The key that signs the release APK. Android installs an update over an installed
// app only if both are signed by the same key, so every APK that goes to anyone has
// to come from this one key, for as long as the app lives. It is made once by
// scripts\make-android-key.ps1, kept in state\ with the other keys (never in the
// repository), and has to be backed up with them.
//
// Without it, release builds fall back to the debug key so that `flutter run
// --release` still works on a development machine. scripts\build-android.ps1
// sets REBIRTH_REQUIRE_RELEASE_KEY and refuses to build without the real key.
val signingProperties = file(
    System.getenv("REBIRTH_SIGNING_PROPERTIES")
        ?: rootProject.projectDir.resolve("../../state/android-signing/key.properties").canonicalPath
)
val haveReleaseKey = signingProperties.exists()
val signing = Properties().apply {
    if (haveReleaseKey) signingProperties.inputStream().use { load(it) }
}

android {
    namespace = "org.awgrotator.awg_client"
    compileSdk = flutter.compileSdkVersion
    ndkVersion = flutter.ndkVersion

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = JavaVersion.VERSION_17.toString()
    }

    defaultConfig {
        // TODO: Specify your own unique Application ID (https://developer.android.com/studio/build/application-id.html).
        applicationId = "app.rebirth.vpn"
        // You can update the following values to match your application needs.
        // For more information, see: https://flutter.dev/to/review-gradle-config.
        minSdk = flutter.minSdkVersion
        targetSdk = flutter.targetSdkVersion
        versionCode = flutter.versionCode
        versionName = flutter.versionName
    }

    signingConfigs {
        if (haveReleaseKey) {
            create("release") {
                // The key file is named relative to the properties file.
                storeFile = signingProperties.parentFile.resolve(signing.getProperty("storeFile"))
                storePassword = signing.getProperty("storePassword")
                keyAlias = signing.getProperty("keyAlias")
                keyPassword = signing.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            signingConfig = if (haveReleaseKey) {
                signingConfigs.getByName("release")
            } else {
                if (System.getenv("REBIRTH_REQUIRE_RELEASE_KEY") == "1") {
                    throw GradleException(
                        "There is no release key (${signingProperties}). " +
                            "Make it with scripts\\make-android-key.ps1."
                    )
                }
                signingConfigs.getByName("debug")
            }
        }
    }
}

flutter {
    source = "../.."
}
