package com.gustavomartins.codexpool

import android.os.Bundle
import android.content.Intent
import com.gustavomartins.codexpool.data.PairingLinks
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import com.gustavomartins.codexpool.ui.PoolApp

class MainActivity : ComponentActivity() {
    private val scanner = registerForActivityResult(ScanContract()) { result ->
        result.contents?.let { PairingLinks.receive(it) }
    }
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        intent.dataString?.let(PairingLinks::receive)
    }
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if (savedInstanceState == null) intent.dataString?.let(PairingLinks::receive)
        enableEdgeToEdge()
        val pool = application as CodexPoolApplication
        setContent { PoolApp(pool.credentials, pool.client, onScanPairing = {
            scanner.launch(ScanOptions().setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                .setPrompt("Leia o QR do runnerd no computador").setBeepEnabled(false).setOrientationLocked(false))
        }) }
    }
}
