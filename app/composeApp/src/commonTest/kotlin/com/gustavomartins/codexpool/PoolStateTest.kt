@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import app.cash.turbine.test
import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.*
import kotlin.test.*

class PoolStateTest {
    private val session = Session("s-1", Agent.Codex, "m", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated, SessionState.Running)
    private val credentials = Credentials("https://pool.example", "test-token", "app-client")
    @BeforeTest fun mainDispatcher() { Dispatchers.setMain(UnconfinedTestDispatcher()) }
    @AfterTest fun resetMainDispatcher() { Dispatchers.resetMain() }

    @Test fun turnDeltasMergeOnlyWithinSameTurnAndKind() {
        var state = SessionScreenState(session = session)
        state = reduceEvent(state, SessionEvent.Delta(TurnDelta("t-1", DeltaKind.Text, "hel")))
        state = reduceEvent(state, SessionEvent.Delta(TurnDelta("t-1", DeltaKind.Text, "lo")))
        state = reduceEvent(state, SessionEvent.Delta(TurnDelta("t-1", DeltaKind.ToolCall, "ls")))
        state = reduceEvent(state, SessionEvent.Delta(TurnDelta("t-2", DeltaKind.Text, "new")))
        assertEquals(listOf("hello", "ls", "new"), state.entries.map { it.content })
    }

    @Test fun approvalsAreDeduplicatedAndResolvedByAuthoritativeEvents() {
        val approval = Approval("a-1", "git status", "Workspace /work")
        var state = SessionScreenState(session = session)
        repeat(2) { state = reduceEvent(state, SessionEvent.Requested(approval)) }
        assertEquals(SessionState.WaitingApproval, state.session?.state)
        assertEquals(1, state.approvals.size)
        state = reduceEvent(state, SessionEvent.Resolved(ApprovalResolved("a-1", Decision.Deny)))
        assertTrue(state.approvals.isEmpty())
        state = reduceEvent(state, SessionEvent.State(StateEvent(SessionState.Running)))
        assertEquals(SessionState.Running, state.session?.state)
    }

    @Test fun disconnectedAndOrphanedSessionsCannotSendOrCancel() {
        val ready = SessionScreenState(session = session.copy(state = SessionState.WaitingInput), connection = StreamConnection.Live)
        assertTrue(ready.canSend)
        assertFalse(ready.copy(connection = StreamConnection.Disconnected).canSend)
        assertFalse(ready.copy(session = session.copy(state = SessionState.Orphaned)).canSend)
        assertFalse(ready.copy(busy = true).canSend)
        assertFalse(SessionScreenState().canCancel)
    }

    @Test fun closedSessionClearsPendingApprovalsAndDisablesInput() {
        val state = reduceEvent(SessionScreenState(session = session, connection = StreamConnection.Live,
            approvals = listOf(Approval("a", "command", "context"))), SessionEvent.Closed(ClosedEvent("process_exit")))
        assertEquals(SessionState.Closed, state.session?.state)
        assertEquals("process_exit", state.session?.closedReason)
        assertTrue(state.approvals.isEmpty())
        assertFalse(state.canSend)
        assertFalse(state.canCancel)
    }

    @Test fun encryptedStorageFailureDoesNotAuthenticateTheApp() = runTest {
        val store = object : CredentialStore {
            override suspend fun read(): Credentials? = error("unreadable ciphertext")
            override suspend fun write(credentials: Credentials) = error("unavailable key")
            override suspend fun clear() {}
        }
        val client = HttpClient(MockEngine { error("No request expected") }) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, credentials.baseUrl)
            vm.state.test {
                val failed = awaitItem()
                assertNull(failed.credentials)
                assertNotNull(failed.error)
                assertFalse(failed.loading)
            }
        } finally { client.close() }
    }

    @Test fun loginPublishesCredentialsOnlyAfterSecureStorageSucceeds() = runTest {
        val store = object : CredentialStore {
            override suspend fun read(): Credentials? = null
            override suspend fun write(credentials: Credentials) = error("write failed")
            override suspend fun clear() {}
        }
        val engine = MockEngine { respond("""{"token":"test-token","client_id":"mobile"}""", HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json")) }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, credentials.baseUrl)
            vm.login("user", "password")
            val failed = vm.state.first { !it.loading }
            assertNull(failed.credentials)
            assertNotNull(failed.error)
        } finally { client.close() }
    }

    @Test fun sessionFetchErrorLeavesControlsDisconnected() = runTest {
        val client = HttpClient(MockEngine { respond("", HttpStatusCode.Forbidden) }) { poolConfiguration() }
        try {
            val vm = SessionViewModel(PoolApi(client, credentials), "s-1")
            vm.connect()
            val failed = vm.state.first { it.connection == StreamConnection.Disconnected && it.error != null }
            assertFalse(failed.canSend)
            assertNotNull(failed.error)
        } finally { client.close() }
    }

    @Test fun credentialsForAnotherBuildOriginCannotAuthenticate() = runTest {
        val store = object : CredentialStore {
            override suspend fun read() = credentials.copy(baseUrl = "https://other.example")
            override suspend fun write(credentials: Credentials) {}
            override suspend fun clear() {}
        }
        val engine = MockEngine { error("The stored origin must never receive a request") }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, credentials.baseUrl)
            assertNull(vm.state.value.credentials)
            assertNotNull(vm.state.value.error)
            assertTrue(engine.requestHistory.isEmpty())
        } finally { client.close() }
    }

    @Test fun logoutRevokesTheAppCredentialBeforeClearingStorage() = runTest {
        var cleared = false
        val store = object : CredentialStore {
            override suspend fun read() = credentials
            override suspend fun write(credentials: Credentials) {}
            override suspend fun clear() { cleared = true }
        }
        val engine = MockEngine { request ->
            assertFalse(cleared)
            assertEquals(HttpMethod.Delete, request.method)
            assertEquals("/api/auth/token", request.url.encodedPath)
            assertEquals("Bearer test-token", request.headers[HttpHeaders.Authorization])
            respond("", HttpStatusCode.NoContent)
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, credentials.baseUrl)
            vm.logout()
            val loggedOut = vm.state.first { !it.loading }
            assertNull(loggedOut.credentials)
            assertTrue(cleared)
        } finally { client.close() }
    }

    @Test fun failedRevocationIsReportedAndKeepsTheCredential() = runTest {
        var cleared = false
        val store = object : CredentialStore {
            override suspend fun read() = credentials
            override suspend fun write(credentials: Credentials) {}
            override suspend fun clear() { cleared = true }
        }
        val client = HttpClient(MockEngine { respond("", HttpStatusCode.ServiceUnavailable) }) { poolConfiguration() }
        try {
            val vm = AuthViewModel(store, client, credentials.baseUrl)
            vm.logout()
            val failed = vm.state.first { !it.loading }
            assertEquals(credentials, failed.credentials)
            assertNotNull(failed.error)
            assertFalse(cleared)
        } finally { client.close() }
    }
}
