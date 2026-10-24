@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withContext
import kotlinx.coroutines.test.*
import kotlin.test.*

class ConversationActionsTest {
    @BeforeTest fun dispatcher() { Dispatchers.setMain(UnconfinedTestDispatcher()) }
    @AfterTest fun resetDispatcher() { Dispatchers.resetMain() }
    private val credentials = Credentials("https://pool.example", "actions-token", "client")
    private val headers = headersOf(HttpHeaders.ContentType, "application/json")
    private val session = Session("execution", Agent.Codex, "model", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated, SessionState.Closed, conversationId = "conversation")
    private val conversation = Conversation("conversation", "execution", Agent.Codex, "model", "Title", ConversationStorage.Archived, SessionState.Closed, null, emptyList(), true, false)

    @Test fun readingAnArchiveDoesNotResumeAndFailedResumeKeepsTheTranscript() = runTest {
        var resumes = 0
        val client = HttpClient(MockEngine { request ->
            if (request.url.encodedPath.endsWith("/resume")) {
                resumes++
                assertEquals(HttpMethod.Post, request.method)
                respond("", HttpStatusCode.Conflict)
            } else {
                assertEquals("/api/conversations/conversation/history", request.url.encodedPath)
                val events = listOf(HistoryEvent(1, "turn.delta", PoolJson.parseToJsonElement("""{"turn_id":"t","kind":"text","content":"Retained answer"}""")))
                respond(PoolJson.encodeToString(HistoryPage(conversation, session, events, 1, 1, false)), HttpStatusCode.OK, headers)
            }
        }) { poolConfiguration() }
        try {
            val vm = SessionViewModel(PoolApi(client, credentials), "conversation")
            vm.connect(); vm.state.first { it.session != null && it.connection == StreamConnection.Disconnected }
            assertEquals(0, resumes)
            assertEquals("Retained answer", vm.state.value.entries.single().content)
            vm.resume(); vm.state.first { !it.busy }
            assertEquals(1, resumes)
            assertNotNull(vm.state.value.error)
            assertEquals("Retained answer", vm.state.value.entries.single().content)
            assertEquals(ConversationStorage.Archived, vm.state.value.conversation?.storage)
        } finally { client.close() }
    }

    @Test fun deletionWaitsForCanonicalNotFoundAndReportsAnExplicitRunnerFailure() = runTest {
        for (failure in listOf(false, true)) {
            var deleted = false
            val engine = MockEngine { request ->
                assertEquals("/api/conversations/conversation", request.url.encodedPath)
                assertEquals("Bearer actions-token", request.headers[HttpHeaders.Authorization])
                if (request.method == HttpMethod.Delete) { deleted = true; respond("", HttpStatusCode.Accepted) }
                else if (failure) respond(PoolJson.encodeToString(conversation.copy(deleteError = "Runner deletion failed")), HttpStatusCode.OK, headers)
                else { check(deleted); respond("", HttpStatusCode.NotFound) }
            }
            val client = HttpClient(engine) { poolConfiguration() }
            try {
                withContext(Dispatchers.Default) {
                    if (failure) assertEquals("Runner deletion failed", assertFailsWith<SessionProtocolException> { PoolApi(client, credentials).deleteConversation("conversation") }.message)
                    else PoolApi(client, credentials).deleteConversation("conversation")
                }
                assertEquals(2, engine.requestHistory.size)
            } finally { client.close() }
        }
    }
}
