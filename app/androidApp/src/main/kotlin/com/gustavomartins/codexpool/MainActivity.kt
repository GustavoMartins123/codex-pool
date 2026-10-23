package com.gustavomartins.codexpool

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import com.gustavomartins.codexpool.ui.PoolApp

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        val pool = application as CodexPoolApplication
        setContent { PoolApp(pool.credentials, pool.client) }
    }
}
