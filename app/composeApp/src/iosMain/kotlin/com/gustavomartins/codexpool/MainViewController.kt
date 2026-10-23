package com.gustavomartins.codexpool

import androidx.compose.ui.window.ComposeUIViewController
import com.gustavomartins.codexpool.data.KeychainCredentialStore
import com.gustavomartins.codexpool.data.poolConfiguration
import com.gustavomartins.codexpool.ui.PoolApp
import io.ktor.client.HttpClient
import io.ktor.client.engine.darwin.Darwin

fun MainViewController(): platform.UIKit.UIViewController {
    val store = KeychainCredentialStore()
    val client = HttpClient(Darwin) { poolConfiguration() }
    return ComposeUIViewController { PoolApp(store, client) }
}
