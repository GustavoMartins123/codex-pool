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

class ConversationListTest {
    @BeforeTest fun mainDispatcher() { Dispatchers.setMain(UnconfinedTestDispatcher()) }
    @AfterTest fun resetDispatcher() { Dispatchers.resetMain() }
    private val headers = headersOf(HttpHeaders.ContentType, "application/json")
    private val credentials = Credentials("https://pool.example", "test-token", "client")
    private fun conversation(id: String, storage: ConversationStorage) = Conversation(id, "execution-$id", Agent.Codex,
        "model", "Title $id", storage, SessionState.Closed, null, listOf(MessagePreview("t", "assistant", "Preview $id")), false, false)

    @Test fun loadingMoreUsesTheCursorAndSwitchingCategoriesReplacesTheList() = runTest {
        val engine = MockEngine { request ->
            assertEquals("/api/conversations", request.url.encodedPath)
            assertEquals("Bearer test-token", request.headers[HttpHeaders.Authorization])
            assertEquals("50", request.url.parameters["limit"])
            val page = when {
                request.url.parameters["storage"] == "archived" -> ConversationPage(listOf(conversation("old", ConversationStorage.Archived)))
                request.url.parameters["cursor"] == "page-two" -> ConversationPage(listOf(conversation("second", ConversationStorage.Active)))
                else -> ConversationPage(listOf(conversation("first", ConversationStorage.Active)), "page-two")
            }
            respond(PoolJson.encodeToString(page), HttpStatusCode.OK, headers)
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val vm = DashboardViewModel(PoolApi(client, credentials))
            vm.state.first { !it.loading }
            assertEquals(listOf("first"), vm.state.value.conversations?.map { it.id })
            vm.loadMoreConversations()
            vm.state.first { !it.loading }
            assertEquals(listOf("first", "second"), vm.state.value.conversations?.map { it.id })
            vm.selectStorage(ConversationStorage.Archived)
            vm.state.first { !it.loading }
            assertEquals(listOf("old"), vm.state.value.conversations?.map { it.id })
            assertNull(vm.state.value.nextCursor)
            assertEquals(3, engine.requestHistory.size)
        } finally { client.close() }
    }

    @Test fun prependingAnEarlierPageKeepsTheVisibleEntryKeyAndStreamingText() {
        val older = listOf(TranscriptEntry("t", "Thinking", "reason", 1), TranscriptEntry("t", "Text", "hello ", 2))
        val current = listOf(TranscriptEntry("t", "Text", "world plus new live output", 20), TranscriptEntry("t", "summary", "42 tokens", 30))
        val combined = prependTranscript(older, current)
        assertEquals("hello world plus new live output", combined[1].content)
        assertEquals(20L, combined[1].keyEventId)
        assertEquals(listOf(1L, 20L, 30L), transcriptSections(combined).map { it.id })
        assertEquals(3, combined.size)
    }
}
