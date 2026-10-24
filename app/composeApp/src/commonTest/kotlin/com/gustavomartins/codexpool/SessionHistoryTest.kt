@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.*
import kotlin.test.*

class SessionHistoryTest {
    private val credentials = Credentials("https://pool.example", "history-token", "client")
    private val session = Session("s", Agent.Codex, "model", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated, SessionState.Closed)
    private val headers = headersOf(HttpHeaders.ContentType, "application/json")
    @BeforeTest fun mainDispatcher() { Dispatchers.setMain(UnconfinedTestDispatcher()) }
    @AfterTest fun resetDispatcher() { Dispatchers.resetMain() }

    @Test fun reopeningClosedSessionLoadsEveryPageAndRestoresUserThinkingToolsReplyAndSummary() = runTest {
        var requests = 0
        val engine = MockEngine { request ->
            assertEquals("/api/sessions/s/history", request.url.encodedPath)
            assertEquals("Bearer history-token", request.headers[HttpHeaders.Authorization])
            val first = request.url.parameters["after"] == "0"
            if (first) assertNull(request.url.parameters["through"]) else assertEquals("5", request.url.parameters["through"])
            val events = if (first) """[
                {"id":1,"type":"turn.input","data":{"turn_id":"t","input":"User question"}},
                {"id":2,"type":"turn.delta","data":{"turn_id":"t","kind":"thinking","content":"Analysis"}}
            ]""" else """[
                {"id":3,"type":"turn.delta","data":{"turn_id":"t","kind":"tool_call","content":"Tool"}},
                {"id":4,"type":"turn.delta","data":{"turn_id":"t","kind":"text","content":"Answer"}},
                {"id":5,"type":"turn.finished","data":{"turn_id":"t","tokens":42,"duration_ms":123}}
            ]"""
            requests++
            respond("""{"session":${PoolJson.encodeToString(session)},"events":$events,"cursor":${if (first) 2 else 5},"through":5,"has_more":$first}""", HttpStatusCode.OK, headers)
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            repeat(2) {
                val vm = SessionViewModel(PoolApi(client, credentials), "s")
                vm.connect()
                vm.state.first { it.connection == StreamConnection.Disconnected && it.session != null }
                assertEquals(listOf("user", "Thinking", "ToolCall", "Text", "summary"), vm.state.value.entries.map { it.kind })
                assertEquals(listOf("User question", "Analysis", "Tool", "Answer"), vm.state.value.entries.take(4).map { it.content })
                assertEquals(SessionState.Closed, vm.state.value.session?.state)
                assertEquals(StreamConnection.Disconnected, vm.state.value.connection)
                assertNull(vm.state.value.error)
                vm.disconnect()
            }
            assertEquals(4, requests)
        } finally { client.close() }
    }

    @Test fun gapsAndUnavailableHistoryFailBeforeOpeningAnyOtherEndpoint() = runTest {
        for (status in listOf(HttpStatusCode.OK, HttpStatusCode.Conflict, HttpStatusCode.NotFound)) {
            val engine = MockEngine {
                respond("""{"session":${PoolJson.encodeToString(session)},"events":[{"id":2,"type":"state","data":{"state":"closed"}}],"cursor":2,"through":2,"has_more":false}""", status, headers)
            }
            val client = HttpClient(engine) { poolConfiguration() }
            try {
                val vm = SessionViewModel(PoolApi(client, credentials), "s")
                vm.connect()
                vm.state.first { it.error != null }
                assertNotNull(vm.state.value.error)
                assertTrue(vm.state.value.entries.isEmpty())
                assertFalse(vm.state.value.canSend)
                assertEquals(1, engine.requestHistory.size)
            } finally { client.close() }
        }
    }
}
