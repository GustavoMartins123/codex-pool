import org.jetbrains.kotlin.gradle.dsl.JvmTarget
import java.net.URI

plugins {
    alias(libs.plugins.kotlin.multiplatform)
    alias(libs.plugins.android.multiplatform.library)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose.multiplatform)
    alias(libs.plugins.kotlin.serialization)
}

val generatedPoolConfig = layout.buildDirectory.dir("generated/poolConfig/commonMain")
val generatePoolConfig = tasks.register("generatePoolConfig") {
    val environment = rootProject.layout.projectDirectory.file(".env")
    inputs.files(environment)
    outputs.dir(generatedPoolConfig)
    doLast {
        check(environment.asFile.isFile) { "Create app/.env with POOL_URL=https://your-pool-host before building the app." }
        val entries = environment.asFile.readLines(Charsets.UTF_8)
            .map { it.trim() }.filter { it.isNotEmpty() && !it.startsWith("#") }
            .map { line ->
                check('=' in line) { "Invalid entry in app/.env: expected KEY=value." }
                line.substringBefore('=').trim() to line.substringAfter('=').trim()
            }
        val urls = entries.filter { it.first == "POOL_URL" }
        check(urls.size == 1) { "app/.env must declare POOL_URL exactly once." }
        val value = urls.single().second.removeSurrounding("\"").removeSurrounding("'")
        val uri = URI(value)
        check(uri.scheme == "https" && !uri.host.isNullOrBlank() && (uri.port == -1 || uri.port in 1..65535) && uri.rawUserInfo == null &&
            uri.rawQuery == null && uri.rawFragment == null && (uri.rawPath.isEmpty() || uri.rawPath == "/")) {
            "POOL_URL must be an HTTPS origin without credentials, path, query or fragment."
        }
        val target = generatedPoolConfig.get().file("com/gustavomartins/codexpool/data/AppBuildConfig.kt").asFile
        target.parentFile.mkdirs()
        target.writeText("package com.gustavomartins.codexpool.data\n\nobject AppBuildConfig { const val PoolUrl = \"${uri.toASCIIString().trimEnd('/')}\" }\n")
    }
}

kotlin {
    android {
        namespace = "com.gustavomartins.codexpool.shared"
        compileSdk = 37
        minSdk = 26
        androidResources { enable = true }
        withHostTest {}
        compilerOptions { jvmTarget.set(JvmTarget.JVM_17) }
    }
    listOf(iosArm64(), iosSimulatorArm64()).forEach { target ->
        target.binaries.framework {
            baseName = "ComposeApp"
            isStatic = true
        }
    }
    // Runs shared contract and state tests on hosts without Xcode.
    jvm { compilerOptions { jvmTarget.set(JvmTarget.JVM_17) } }
    sourceSets {
        commonMain { kotlin.srcDir(generatePoolConfig) }
        commonMain.dependencies {
            implementation(libs.compose.runtime)
            implementation(libs.compose.foundation)
            implementation(libs.compose.material3)
            implementation(libs.compose.ui)
            implementation(libs.compose.resources)
            implementation(libs.kotlinx.coroutines.core)
            implementation(libs.kotlinx.serialization.json)
            implementation(libs.ktor.client.core)
            implementation(libs.ktor.client.content.negotiation)
            implementation(libs.ktor.serialization.kotlinx.json)
            implementation(libs.koin.core)
            implementation(libs.koin.compose)
            implementation(libs.koin.compose.viewmodel)
            implementation(libs.navigation.compose)
            implementation(libs.lifecycle.viewmodel)
            implementation(libs.lifecycle.runtime.compose)
        }
        androidMain.dependencies {
            implementation(libs.ktor.client.okhttp)
            implementation(libs.datastore.preferences)
        }
        iosMain.dependencies { implementation(libs.ktor.client.darwin) }
        jvmTest.dependencies {
            implementation(libs.ktor.client.cio)
            implementation(libs.ktor.client.okhttp)
        }
        commonTest.dependencies {
            implementation(kotlin("test"))
            implementation(libs.kotlinx.coroutines.test)
            implementation(libs.ktor.client.mock)
            implementation(libs.turbine)
        }
    }
}
