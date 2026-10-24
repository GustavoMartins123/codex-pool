package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import io.ktor.http.content.TextContent
import kotlinx.coroutines.test.runTest
import kotlin.test.*

class ModelSettingsTest {
    @Test fun modelCapabilitiesUseTheCatalogProtocolsAndEfforts() {
        val model = PoolJson.decodeFromString<CatalogModel>("""{"id":"gemini-test","protocol":"gemini","protocols":["responses","anthropic"],"provider":"antigravity","available_now":true,"reasoning_efforts":["minimal","low","high"]}""")
        assertTrue(model.supports(Agent.Codex))
        assertTrue(model.supports(Agent.Claude))
        assertEquals(listOf("minimal", "low", "high"), model.efforts(Agent.Codex))
        assertEquals(listOf("low", "high"), model.efforts(Agent.Claude))
        val nativeGemini = model.copy(protocols = emptyList())
        assertFalse(nativeGemini.supports(Agent.Codex))
        assertFalse(nativeGemini.supports(Agent.Claude))
        assertTrue(model.copy(reasoningEfforts = emptyList()).efforts(Agent.Codex).isEmpty())
    }

    @Test fun turnCarriesTheSelectedModelAndEffortInOneRequest() = runTest {
        val engine = MockEngine { request ->
            assertEquals("/api/sessions/session/turns", request.url.encodedPath)
            assertEquals(HttpMethod.Post, request.method)
            assertEquals("Bearer token", request.headers[HttpHeaders.Authorization])
            val input = PoolJson.decodeFromString<TurnInput>((request.body as TextContent).text)
            assertEquals(TurnInput("Continue the same conversation", "another-model", "high"), input)
            respond("""{"turn_id":"next-turn"}""", headers = headersOf(HttpHeaders.ContentType, "application/json"))
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val api = PoolApi(client, Credentials("https://pool.example", "token", "client"))
            assertEquals("next-turn", api.turn("session", "Continue the same conversation", TurnSettings("another-model", "high")).turnId)
            assertEquals(1, engine.requestHistory.size)
        } finally { client.close() }
    }

    @Test fun settingsEventsUpdateTheSessionWithoutDiscardingHistoryOrDraft() {
        val session = Session("session", Agent.Codex, "old-model", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated, SessionState.WaitingInput, conversationId = "conversation")
        val draft = TurnSettings("future-model", "low")
        val original = SessionScreenState(session = session, entries = listOf(TranscriptEntry("previous-turn", "Text", "Earlier reply", 1)), selectedSettings = draft)
        val updated = reduceEvent(original, decodeEvent("session.settings", """{"model":"new-model","reasoning_effort":"high"}"""), 2)
        assertEquals("new-model", updated.session?.model)
        assertEquals("high", updated.session?.reasoningEffort)
        assertEquals("conversation", updated.session?.conversationId)
        assertEquals(original.entries, updated.entries)
        assertEquals(draft, updated.turnSettings)
    }
}
