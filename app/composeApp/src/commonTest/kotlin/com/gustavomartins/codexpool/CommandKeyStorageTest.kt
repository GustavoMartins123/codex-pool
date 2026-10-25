@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import io.ktor.http.content.TextContent
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.*
import kotlin.test.*

class CommandKeyStorageTest {
    @BeforeTest fun dispatcher() { Dispatchers.setMain(UnconfinedTestDispatcher()) }
    @AfterTest fun resetDispatcher() { Dispatchers.resetMain() }
    private val url = "https://pool.example"
    private fun key(seed: Int) = base64UrlEncode(ByteArray(CommandSigning.KEY_BYTES) { (seed + it).toByte() })

    @Test fun keysStayWithTheirPoolDeduplicatedAndBounded() {
        val other = "https://other.example"
        var accounts = PoolAccounts(selectedUrl = url, providers = listOf(PoolProvider("A", url), PoolProvider("B", other))).validate()
        accounts = accounts.withCommandKey(key(1)).withCommandKey(key(1))
        assertEquals(listOf(key(1)), accounts.selected.commandKeys)
        assertTrue(accounts.providers.single { it.baseUrl == other }.commandKeys.isEmpty())
        for (seed in 2..40) accounts = accounts.withCommandKey(key(seed))
        assertEquals(MAX_COMMAND_KEYS, accounts.selected.commandKeys.size)
        assertEquals(key(40), accounts.selected.commandKeys.last())
        assertEquals(accounts, decodeStoredAccounts(PoolJson.encodeToString(accounts)).accounts)
        assertTrue(decodeStoredAccounts("""{"version":2,"selectedUrl":"$url","providers":[{"name":"A","baseUrl":"$url"}]}""")
            .accounts.selected.commandKeys.isEmpty())
        assertFails { accounts.copy(providers = listOf(PoolProvider("A", url, commandKeys = listOf("not-a-key")))).validate() }
    }

    @Test fun pairingKeepsTheRunnerKeyBeforeApprovingAndSignedSessionsHoldTheFirstPrompt() = runTest {
        val requestId = "a".repeat(43)
        val events = mutableListOf<String>()
        var created: CreateSession? = null
        val headers = headersOf(HttpHeaders.ContentType, "application/json")
        val engine = MockEngine { request ->
            when (request.url.encodedPath) {
                "/api/conversations" -> respond(PoolJson.encodeToString(ConversationPage(emptyList())), HttpStatusCode.OK, headers)
                "/api/me/runner-pairings/$requestId" -> if (request.method == HttpMethod.Get) {
                    respond(PoolJson.encodeToString(PairingReview(requestId, "desktop", "linux", "amd64", expiresAt = "2026-10-25T00:00:00Z")), HttpStatusCode.OK, headers)
                } else { events += "approved"; respond("", HttpStatusCode.NoContent) }
                "/api/pool/catalog" -> respond(PoolJson.encodeToString(Catalog(listOf(CatalogModel("model-1", protocol = "anthropic", availableNow = true)))), HttpStatusCode.OK, headers)
                "/api/sessions" -> {
                    created = PoolJson.decodeFromString<CreateSession>((request.body as TextContent).text)
                    respond(PoolJson.encodeToString(Session("s-1", Agent.Claude, "model-1", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated,
                        SessionState.PendingRunner, conversationId = "s-1")), HttpStatusCode.Created, headers)
                }
                else -> error("Unexpected request: ${request.url}")
            }
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val stored = mutableListOf<String>()
            val signer = CommandSigner(keys = { stored.toList() }, store = { events += "stored"; stored += it })
            val vm = DashboardViewModel(PoolApi(client, Credentials(url, "token", "client")), signer)
            vm.state.first { !it.loading }
            val runnerKey = key(9)
            vm.reviewPairing("codexpool://pair?pool=https%3A%2F%2Fpool.example&request=$requestId&key=$runnerKey")
            vm.state.first { it.pairing != null && !it.loading }
            vm.decidePairing(requestId, Decision.Approve, "AbCd1234")
            vm.state.first { it.pairingComplete && !it.loading }
            assertEquals(listOf("stored", "approved"), events)
            assertEquals(listOf(runnerKey), stored)

            vm.loadCatalog(); vm.state.first { it.catalog != null && !it.loading }
            var opened: String? = null
            vm.create(CreateSession(Agent.Claude, "model-1", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated, prompt = "list files")) { opened = it }
            vm.state.first { opened != null && !it.loading }
            assertNull(created?.prompt)
            assertEquals("list files", PendingPrompts.take("s-1"))
        } finally { client.close() }
    }
}
