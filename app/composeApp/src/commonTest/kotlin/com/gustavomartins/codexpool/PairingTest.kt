package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import kotlinx.coroutines.test.runTest
import kotlin.test.*

class PairingTest {
    private val id = "a".repeat(43)
    private val link = "codexpool://pair?pool=https%3A%2F%2Fpool.example&request=$id"
    @Test fun linksAreBoundToTheConfiguredPoolAndCannotInjectPaths() {
        assertEquals(id, pairingRequestId(link, "https://pool.example"))
        for (value in listOf(link.replace("pool.example", "attacker.example"), "$link&request=$id", "$link#secret",
            link.replace("request=$id", "request=../credentials"), link.replace("codexpool://pair", "https://pair"))) {
            assertFailsWith<IllegalArgumentException> { pairingRequestId(value, "https://pool.example") }
        }
    }
    @Test fun currentRunnerQrCarriesItsCommandKeyAndRejectsMalformedKeys() {
        val key = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
        assertEquals(PairingQr(id, key), parsePairingLink("$link&key=$key", "https://pool.example"))
        assertEquals(PairingQr(id, null), parsePairingLink(link, "https://pool.example"))
        for (value in listOf("$link&key=short", "$link&key=$key&key=$key", "$link&extra=1")) {
            assertFailsWith<IllegalArgumentException> { parsePairingLink(value, "https://pool.example") }
        }
    }
    @Test fun foreignQrNeverSendsAppCredentialsAndApprovalUsesMachineCode() = runTest {
        var calls = 0
        val client = HttpClient(MockEngine { request ->
            calls++
            assertEquals("pool.example", request.url.host)
            assertEquals("Bearer app-only", request.headers[HttpHeaders.Authorization])
            respond("", HttpStatusCode.NoContent)
        }) { poolConfiguration() }
        try {
            val api = PoolApi(client, Credentials("https://pool.example", "app-only", "client"))
            assertFailsWith<IllegalArgumentException> { api.reviewPairing(link.replace("pool.example", "attacker.example")) }
            assertEquals(0, calls)
            assertFailsWith<IllegalArgumentException> { api.decidePairing(id, Decision.Approve, "") }
            assertEquals(0, calls)
            api.decidePairing(id, Decision.Approve, "AbCd1234")
            assertEquals(1, calls)
        } finally { client.close() }
    }
}
