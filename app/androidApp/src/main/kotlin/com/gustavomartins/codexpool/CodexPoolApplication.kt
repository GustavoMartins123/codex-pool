package com.gustavomartins.codexpool

import android.app.Application
import com.gustavomartins.codexpool.data.AndroidCredentialStore
import com.gustavomartins.codexpool.data.poolConfiguration
import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp

class CodexPoolApplication : Application() {
    val credentials by lazy { AndroidCredentialStore(this) }
    val client by lazy { HttpClient(OkHttp) { poolConfiguration() } }
}
