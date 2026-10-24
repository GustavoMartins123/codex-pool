package com.gustavomartins.codexpool

import android.Manifest
import androidx.test.core.app.ActivityScenario
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.journeyapps.barcodescanner.CaptureActivity
import com.journeyapps.barcodescanner.DecoratedBarcodeView
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class PairingCameraTest {
    @Test fun qrScannerOpensThePhysicalCamera() {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val context = instrumentation.targetContext
        instrumentation.uiAutomation.grantRuntimePermission(context.packageName, Manifest.permission.CAMERA)
        val intent = ScanContract().createIntent(context, ScanOptions().setDesiredBarcodeFormats(ScanOptions.QR_CODE)
            .setPrompt("Leia o QR do runnerd no computador").setBeepEnabled(false))
        ActivityScenario.launch<CaptureActivity>(intent).use { scenario ->
            var active = false
            val deadline = System.nanoTime() + 5_000_000_000L
            while (!active && System.nanoTime() < deadline) {
                scenario.onActivity { activity ->
                    val scanner = activity.findViewById<DecoratedBarcodeView>(com.google.zxing.client.android.R.id.zxing_barcode_scanner)
                    active = scanner.barcodeView.isPreviewActive
                }
                if (!active) Thread.sleep(50)
            }
            assertTrue("O scanner deve abrir a câmera do aparelho", active)
        }
    }
}
