package com.gustavomartins.codexpool

import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.PoolApp
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import org.junit.After
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.koin.core.context.stopKoin
import java.util.concurrent.atomic.AtomicBoolean

@RunWith(AndroidJUnit4::class)
class ConversationNavigationTest {
    @get:Rule val compose = createComposeRule()
    private val resumed = AtomicBoolean(false)
    private val deleted = AtomicBoolean(false)
    private val session = """{"id":"execution-old","conversation_id":"old","agent":"codex","model":"model","workspace":{"mode":"ephemeral"},"permission_mode":"gated","state":"closed"}"""
    private fun conversation(id: String, storage: String) = """{"id":"$id","session_id":"execution-$id","agent":"codex","model":"model","title":"Title $id","storage":"$storage","session_state":"closed","last_interaction_at":null,"preview":[{"turn_id":"t","role":"assistant","text":"Preview $id"}],"resume_available":${id == "old" && !resumed.get()},"deleting":false}"""
    private val client = HttpClient(MockEngine { request ->
        val body = when (request.url.encodedPath) {
            "/api/conversations" -> when {
                deleted.get() -> """{"conversations":[]}"""
                request.url.parameters["storage"] == "archived" -> """{"conversations":[${conversation("old", "archived") }]}"""
                request.url.parameters["cursor"] == "next-page" -> """{"conversations":[${conversation("second", "active") }]}"""
                else -> """{"conversations":[${conversation("first", "active")}],"next_cursor":"next-page"}"""
            }
            "/api/conversations/old/history" -> """{"conversation":${conversation("old", "archived")},"session":$session,"events":[{"id":1,"type":"turn.delta","data":{"turn_id":"t","kind":"text","content":"Archived full response"}}],"before":1,"through":1,"has_older":false}"""
            "/api/conversations/old/resume" -> { check(request.method == HttpMethod.Post); resumed.set(true); session }
            "/api/conversations/old" -> {
                if (request.method == HttpMethod.Delete) { deleted.set(true); return@MockEngine respond("", HttpStatusCode.Accepted) }
                check(deleted.get()); return@MockEngine respond("", HttpStatusCode.NotFound)
            }
            else -> error("Unexpected conversation endpoint: ${request.url}")
        }
        respond(body, HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
    }) { poolConfiguration() }
    private val store = object : CredentialStore {
        override suspend fun read() = PoolAccounts.initial(AppBuildConfig.PoolUrl).withCredentials(Credentials(AppBuildConfig.PoolUrl, "token", "client"))
        override suspend fun write(accounts: PoolAccounts) {}
        override suspend fun clear() {}
    }

    @Test fun resumeIsExplicitAndPermanentDeletionRequiresConfirmation() {
        compose.setContent { PoolApp(store, client) }
        waitForText("Preview first")
        compose.onNodeWithText("Arquivadas").performClick()
        waitForText("Preview old")
        compose.onNodeWithText("old").performClick()
        waitForText("Archived full response")
        check(!resumed.get())
        compose.onNodeWithTag("session-events").performScrollToNode(hasText("Retomar conversa"))
        compose.onNodeWithText("Retomar conversa").performClick()
        compose.waitUntil(10_000) { resumed.get() }
        waitForText("Archived full response")
        compose.onNodeWithText("Apagar").performClick()
        waitForText("Apagar conversa permanentemente?")
        compose.onNodeWithText("Cancelar").performClick()
        check(!deleted.get())
        compose.onNodeWithText("Apagar").performClick()
        compose.onNodeWithText("Apagar permanentemente").performClick()
        waitForText("Nenhuma conversa arquivada")
        check(deleted.get())
    }
    @After fun cleanup() { stopKoin(); client.close() }

    @Test fun previewsPaginationAndArchivedHistoryAreVisibleWithoutRestoringTheConversation() {
        compose.setContent { PoolApp(store, client) }
        waitForText("Preview first")
        compose.onNodeWithText(AppBuildConfig.PoolUrl).assertDoesNotExist()
        compose.onNodeWithText("Carregar mais conversas").performScrollTo().performClick()
        compose.onNode(hasScrollAction()).performScrollToNode(hasText("Preview second"))
        waitForText("Preview second")
        compose.onNode(hasScrollAction()).performScrollToNode(hasText("Arquivadas"))
        compose.onNodeWithText("Arquivadas").performClick()
        waitForText("Preview old")
        compose.onNodeWithText("old").performClick()
        waitForText("Archived full response")
        compose.onNodeWithText("Voltar").performClick()
        waitForText("Preview old")
    }

    private fun waitForText(value: String) {
        compose.waitUntil(10_000) { compose.onAllNodesWithText(value).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText(value).assertIsDisplayed()
    }
}
