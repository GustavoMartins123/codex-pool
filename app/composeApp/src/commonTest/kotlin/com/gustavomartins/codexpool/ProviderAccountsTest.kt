@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.AuthViewModel
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.async
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.*
import kotlin.test.*

class ProviderAccountsTest {
    private val primary = "https://primary.example"
    private val second = "https://second.example"
    @BeforeTest fun mainDispatcher() { Dispatchers.setMain(UnconfinedTestDispatcher()) }
    @AfterTest fun resetDispatcher() { Dispatchers.resetMain() }

    private class Store(var accounts: PoolAccounts? = null) : CredentialStore {
        var failWrite = false
        override suspend fun read() = accounts
        override suspend fun write(accounts: PoolAccounts) { check(!failWrite); this.accounts = accounts }
        override suspend fun clear() { accounts = null }
    }

    @Test fun concurrentCatalogAndPairingWritesPreserveBothUpdates() = runTest {
        val owner = Credentials(primary, "token", "client")
        val entered = CompletableDeferred<Unit>()
        val release = CompletableDeferred<Unit>()
        var saved = PoolAccounts(selectedUrl = primary, providers = listOf(PoolProvider("A", primary, owner)))
        val store = object : CredentialStore {
            override suspend fun read() = saved
            override suspend fun write(accounts: PoolAccounts) {
                if (accounts.selected.runnerCatalog != null && !release.isCompleted) { entered.complete(Unit); release.await() }
                saved = accounts
            }
            override suspend fun clear() {}
        }
        val client = HttpClient(MockEngine { error("No network request expected") }) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, primary)
            vm.state.first { !it.loading }
            val snapshot = RunnerCatalogSnapshot("a".repeat(64), emptyList(), emptyList())
            val cache = async { vm.saveRunnerCatalog(snapshot, owner) }
            entered.await()
            val key = base64UrlEncode(ByteArray(RunnerContract.CommandKeyBytes) { 7 })
            val pairing = async { vm.addCommandKey(key, owner) }
            release.complete(Unit)
            cache.await(); pairing.await()
            assertEquals(snapshot, saved.selected.runnerCatalog?.snapshot)
            assertEquals(listOf(key), saved.selected.commandKeys)
        } finally { release.complete(Unit); client.close() }
    }

    @Test fun switchingAndRestartKeepIndependentTokensAndLogoutOnlyTheSelectedDomain() = runTest {
        val store = Store()
        val engine = MockEngine { request ->
            val host = request.url.host
            when {
                request.method == HttpMethod.Post -> {
                    assertNull(request.headers[HttpHeaders.Authorization])
                    respond("""{"token":"token-$host","client_id":"client-$host"}""", HttpStatusCode.OK,
                        headersOf(HttpHeaders.ContentType, "application/json"))
                }
                request.method == HttpMethod.Delete -> {
                    assertEquals("second.example", host)
                    assertEquals("Bearer token-second.example", request.headers[HttpHeaders.Authorization])
                    respond("", HttpStatusCode.NoContent)
                }
                else -> error("Unexpected request")
            }
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, primary)
            vm.state.first { !it.loading }
            assertEquals(primary, store.accounts?.selectedUrl)
            vm.login("user-a", "password-a")
            vm.state.first { !it.loading }
            vm.addProvider("Second", "$second/")
            assertNull(vm.state.value.credentials)
            vm.login("user-b", "password-b")
            vm.state.first { !it.loading }
            vm.selectProvider(primary)
            assertEquals("token-primary.example", vm.state.value.credentials?.token)
            vm.selectProvider(second)
            val reopened = AuthViewModel(store, client, primary)
            assertEquals(second, reopened.state.value.credentials?.baseUrl)
            reopened.logout()
            reopened.state.first { !it.loading }
            assertNull(store.accounts?.selected?.credentials)
            reopened.selectProvider(primary)
            assertEquals("token-primary.example", reopened.state.value.credentials?.token)
            assertEquals(3, engine.requestHistory.size)
        } finally { client.close() }
    }

    @Test fun invalidOriginsDuplicatesAndStorageFailureDoNotSwitchProvidersOrSendRequests() = runTest {
        val store = Store()
        val engine = MockEngine { error("No requests expected") }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, primary)
            for (url in listOf("http://unsafe.example", "https://user:password@unsafe.example", "$second/path", "$second?token=secret", "$primary/")) {
                vm.addProvider("Invalid", url)
                assertNotNull(vm.state.value.error)
                assertEquals(primary, vm.state.value.accounts?.selectedUrl)
                assertEquals(1, store.accounts?.providers?.size)
            }
            vm.addProvider("Second", second)
            store.failWrite = true
            vm.selectProvider(primary)
            assertEquals(second, vm.state.value.accounts?.selectedUrl)
            assertEquals(second, store.accounts?.selectedUrl)
            assertNotNull(vm.state.value.error)
            assertTrue(engine.requestHistory.isEmpty())
        } finally { client.close() }
    }

    @Test fun oldCredentialIsConvertedOnceAndUnknownOrMismatchedSchemasAreRejected() {
        val credentials = Credentials(primary, "token", "client")
        val converted = decodeStoredAccounts(PoolJson.encodeToString(credentials))
        assertTrue(converted.migrated)
        assertEquals(credentials, converted.accounts.selected.credentials)
        val canonical = decodeStoredAccounts(PoolJson.encodeToString(converted.accounts))
        assertFalse(canonical.migrated)
        assertEquals(converted.accounts, canonical.accounts)
        assertFails { decodeStoredAccounts("""{"version":99,"selectedUrl":"$primary","providers":[]}""") }
        assertFails { PoolAccounts(selectedUrl = primary, providers = listOf(PoolProvider("Bad", primary, credentials.copy(baseUrl = second)))).validate() }
    }
}
