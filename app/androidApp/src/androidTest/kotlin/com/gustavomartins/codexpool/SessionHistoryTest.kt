package com.gustavomartins.codexpool

import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.PoolApp
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import java.util.concurrent.atomic.AtomicInteger
import org.junit.After
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.koin.core.context.stopKoin

@RunWith(AndroidJUnit4::class)
class SessionHistoryTest {
    @get:Rule val compose = createComposeRule()
    private val historyReads = AtomicInteger()
    private val session = """{"id":"s","conversation_id":"s","agent":"codex","model":"history-model","workspace":{"mode":"ephemeral"},"permission_mode":"gated","state":"closed"}"""
    private val conversation = """{"id":"s","session_id":"s","agent":"codex","model":"history-model","title":"","storage":"active","session_state":"closed","last_interaction_at":null,"preview":[],"resume_available":false,"deleting":false}"""
    private val client = HttpClient(MockEngine { request ->
        val body = when (request.url.encodedPath) {
            "/api/conversations" -> """{"conversations":[$conversation]}"""
            "/api/conversations/s/history" -> {
                historyReads.incrementAndGet()
                """{"conversation":$conversation,"session":$session,"before":1,"through":4,"has_older":false,"events":[
                  {"id":1,"type":"turn.input","data":{"turn_id":"t","input":"Pergunta persistida"}},
                  {"id":2,"type":"turn.delta","data":{"turn_id":"t","kind":"thinking","content":"Pensamento persistido"}},
                  {"id":3,"type":"turn.delta","data":{"turn_id":"t","kind":"text","content":"Resposta persistida"}},
                  {"id":4,"type":"turn.finished","data":{"turn_id":"t","tokens":42,"duration_ms":123}}
                ]}"""
            }
            else -> error("Unexpected history test endpoint: ${request.url}")
        }
        respond(body, HttpStatusCode.OK, headersOf(HttpHeaders.ContentType, "application/json"))
    }) { poolConfiguration() }
    private val store = object : CredentialStore {
        override suspend fun read() = PoolAccounts.initial(AppBuildConfig.PoolUrl).withCredentials(Credentials(AppBuildConfig.PoolUrl, "history-token", "client"))
        override suspend fun write(accounts: PoolAccounts) {}
        override suspend fun clear() {}
    }

    @After fun cleanup() { stopKoin(); client.close() }

    @Test fun closedChatRestoresMessagesAndThinkingOnEveryReopen() {
        compose.setContent { PoolApp(store, client) }
        repeat(2) {
            waitForText("s")
            compose.onNodeWithText("s").performClick()
            waitForText("Resposta persistida")
            compose.onNodeWithText("42 tokens · 123 ms").assertIsDisplayed()
            compose.onNodeWithTag("session-events").performScrollToNode(hasText("Pergunta persistida"))
            compose.onNodeWithText("Pergunta persistida").assertIsDisplayed()
            compose.onNodeWithTag("session-events").performScrollToNode(hasText("Pensamento persistido"))
            compose.onNodeWithText("Pensamento persistido").assertIsDisplayed()
            compose.onNodeWithText("Pensando · Ocultar").performClick()
            compose.onNodeWithText("Pensamento persistido").assertDoesNotExist()
            compose.onNodeWithText("Voltar").performClick()
        }
        check(historyReads.get() == 2)
    }

    private fun waitForText(text: String) {
        compose.waitUntil(10_000) { compose.onAllNodesWithText(text).fetchSemanticsNodes().isNotEmpty() }
        compose.onNodeWithText(text).assertIsDisplayed()
    }
}
