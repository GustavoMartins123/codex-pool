package com.gustavomartins.codexpool

import androidx.compose.material3.MaterialTheme
import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.v2.createComposeRule
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import java.io.PipedInputStream
import java.io.PipedOutputStream
import java.util.concurrent.atomic.AtomicReference
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Protocol
import okhttp3.Response
import okhttp3.ResponseBody
import okhttp3.ResponseBody.Companion.toResponseBody
import okio.Buffer
import okio.buffer
import okio.source
import org.junit.Assert.*
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class SessionModelChangeTest {
    @get:Rule val compose = createComposeRule()

    @Test fun chatSendsTheNewModelAndEffortWhileKeepingEarlierMessages() {
        val session = Session("session", Agent.Codex, "gpt-test", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated,
            SessionState.WaitingInput, conversationId = "conversation", runnerId = "runner-1")
        val sent = AtomicReference<TurnInput>()
        val streamWriter = PipedOutputStream()
        val streamReader = PipedInputStream(streamWriter)
        val streamSource = streamReader.source().buffer()
        streamWriter.write(": connected\n\n".toByteArray())
        val client = HttpClient(OkHttp) {
            poolConfiguration()
            engine { addInterceptor { chain ->
                val request = chain.request()
                val jsonType = "application/json".toMediaType()
                val body = when (request.url.encodedPath) {
                "/api/conversations/conversation/history" -> """{
                    "conversation":{"id":"conversation","session_id":"session","agent":"codex","model":"gpt-test","title":"Earlier conversation","storage":"active","session_state":"waiting_input","last_interaction_at":null,"preview":[],"resume_available":false,"deleting":false},
                    "session":${PoolJson.encodeToString(session)},
                    "events":[{"id":1,"type":"turn.delta","data":{"turn_id":"earlier-turn","kind":"text","content":"Earlier response"}}],
                    "before":1,"through":1,"has_older":false
                }""".toResponseBody(jsonType)
                "/api/conversations/conversation/events" -> object : ResponseBody() {
                    override fun contentType() = "text/event-stream".toMediaType()
                    override fun contentLength() = -1L
                    override fun source() = streamSource
                }
                "/api/pool/catalog" -> """{"models":[
                    {"id":"gpt-test","name":"GPT test","protocol":"openai","available_now":true,"reasoning_efforts":["low","high","max"]},
                    {"id":"gemini-test","name":"Gemini test","protocol":"gemini","protocols":["responses"],"available_now":true,"reasoning_efforts":["low","high"]}
                ]}""".toResponseBody(jsonType)
                "/api/me/runners" -> PoolJson.encodeToString(RunnerList(listOf(
                    Runner("runner-1", "desktop", online = true, agents = listOf(Agent.Codex), modelsKnown = true,
                        models = listOf(AgentModel(Agent.Codex, "gpt-test", listOf("low", "high", "max")),
                            AgentModel(Agent.Codex, "gemini-test", listOf("low", "high"))))
                ))).toResponseBody(jsonType)
                "/api/sessions/session/turns" -> {
                    assertEquals("POST", request.method)
                    val buffer = Buffer()
                    requireNotNull(request.body).writeTo(buffer)
                    sent.set(PoolJson.decodeFromString<TurnInput>(buffer.readUtf8()))
                    """{"turn_id":"next-turn"}""".toResponseBody(jsonType)
                }
                else -> error("Unexpected request: ${request.url.encodedPath}")
                }
                Response.Builder().request(request).protocol(Protocol.HTTP_1_1).code(200).message("OK")
                    .header("Content-Type", requireNotNull(body.contentType()).toString()).body(body).build()
            } }
        }
        val signer = CommandSigner(keys = { listOf("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8") }, store = {})
        val vm = SessionViewModel(PoolApi(client, Credentials("https://pool.example", "synthetic", "client")), "conversation", signer)
        try {
            compose.setContent { MaterialTheme { SessionScreen(vm) {} } }
            compose.waitUntil(10_000) { vm.state.value.connection == StreamConnection.Live || vm.state.value.error != null }
            assertEquals(vm.state.value.error, StreamConnection.Live, vm.state.value.connection)
            compose.onNodeWithText("Earlier response").assertIsDisplayed()
            compose.onNodeWithTag("session-model-settings").performClick()
            compose.waitUntil(5_000) { vm.state.value.catalog != null && !vm.state.value.catalogLoading }
            compose.onNodeWithTag("model-selector").performClick()
            compose.onNodeWithText("Gemini test").performClick()
            compose.onNodeWithTag("effort-selector").performClick()
            compose.onNodeWithText("Alto").performClick()
            compose.onNodeWithText("Concluir").performClick()
            compose.onNodeWithText("Mensagem para o agente").performTextInput("Continue here")
            compose.onNodeWithText("Enviar").performClick()
            compose.waitUntil(5_000) { sent.get() != null && !vm.state.value.busy }
            assertEquals(TurnInput("Continue here", "gemini-test", "high"), sent.get().copy(issuedAt = 0, signatures = emptyMap()))
            assertEquals(1, sent.get().signatures.size)
            assertEquals("conversation", vm.state.value.session?.conversationId)
            assertEquals("gemini-test", vm.state.value.session?.model)
            assertEquals("Earlier response", vm.state.value.entries.first().content)
        } finally { vm.disconnect(); streamWriter.close(); streamReader.close(); client.close() }
    }
}
