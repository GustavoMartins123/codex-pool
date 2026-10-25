@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import com.sun.net.httpserver.HttpServer
import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.HttpSend
import io.ktor.client.plugins.plugin
import io.ktor.http.URLProtocol
import java.net.InetSocketAddress
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.*
import kotlin.test.*

class InitialPromptRecoveryTest {
    @BeforeTest fun mainDispatcher() { Dispatchers.setMain(Dispatchers.Unconfined) }
    @AfterTest fun resetDispatcher() { Dispatchers.resetMain() }
    private fun key(seed: Int) = base64UrlEncode(ByteArray(32) { (seed + it).toByte() })

    @Test fun firstPromptSurvivesHttp503AndViewModelRecreation() = runBlocking {
        val release = CountDownLatch(1)
        val turns = AtomicInteger()
        val executor = Executors.newCachedThreadPool()
        val session = """{"id":"audit-first-prompt","conversation_id":"audit-first-prompt","agent":"codex","model":"synthetic","workspace":{"mode":"ephemeral"},"permission_mode":"gated","state":"waiting_input","approvals":[],"pending_input":{"id":"draft-1","input":"Keep this first prompt"}}"""
        val acknowledged = java.util.concurrent.atomic.AtomicBoolean(false)
        val submissions = java.util.concurrent.CopyOnWriteArrayList<TurnInput>()
        val conversation = """{"id":"audit-first-prompt","session_id":"audit-first-prompt","agent":"codex","model":"synthetic","title":"","storage":"active","session_state":"waiting_input","last_interaction_at":null,"preview":[],"resume_available":false,"deleting":false}"""
        val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0).apply {
            this.executor = executor
            createContext("/api/") { exchange ->
                val path = exchange.requestURI.path
                when {
                    path.endsWith("/events") -> {
                        exchange.responseHeaders.set("Content-Type", "text/event-stream")
                        exchange.sendResponseHeaders(200, 0)
                        exchange.responseBody.bufferedWriter().use { writer ->
                            writer.write(": connected\n\n"); writer.flush()
                            release.await(10, TimeUnit.SECONDS)
                        }
                    }
                    path.endsWith("/turns") -> {
                        submissions += PoolJson.decodeFromString<TurnInput>(exchange.requestBody.readBytes().decodeToString())
                        val attempt = turns.incrementAndGet()
                        val body = (if (attempt == 1) """{"error":"temporarily unavailable"}""" else """{"turn_id":"turn-1"}""").toByteArray()
                        if (attempt > 1) acknowledged.set(true)
                        exchange.responseHeaders.set("Content-Type", "application/json")
                        exchange.sendResponseHeaders(if (attempt == 1) 503 else 200, body.size.toLong())
                        exchange.responseBody.use { it.write(body) }
                    }
                    else -> {
                        val session = if (acknowledged.get()) session.replace(",\"pending_input\":{\"id\":\"draft-1\",\"input\":\"Keep this first prompt\"}", "") else session
                        val body = (if (path.endsWith("/history"))
                            """{"conversation":$conversation,"session":$session,"events":[],"before":1,"through":0,"has_older":false}"""
                            else session).toByteArray()
                        exchange.responseHeaders.set("Content-Type", "application/json")
                        exchange.sendResponseHeaders(200, body.size.toLong())
                        exchange.responseBody.use { it.write(body) }
                    }
                }
            }
            start()
        }
        val client = HttpClient(OkHttp) { poolConfiguration() }.apply {
            plugin(HttpSend).intercept { request ->
                request.url.protocol = URLProtocol.HTTP
                request.url.host = "127.0.0.1"
                request.url.port = server.address.port
                execute(request)
            }
        }
        val signer = CommandSigner(keys = { listOf(key(1)) }, store = {})
        val vm = SessionViewModel(PoolApi(client, Credentials("https://pool.example", "synthetic", "client")), "audit-first-prompt", signer)
        val api = PoolApi(client, Credentials("https://pool.example", "synthetic", "client"))
        val recreated = SessionViewModel(api, "audit-first-prompt", signer)
        val reopened = SessionViewModel(api, "audit-first-prompt", signer)
        try {
            vm.connect()
            withTimeout(5_000) { vm.state.first { it.error != null && !it.busy } }
            assertEquals(1, turns.get())
            assertEquals("Keep this first prompt", vm.state.value.session?.pendingInput?.input)
            vm.disconnect()
            recreated.connect()
            withTimeout(5_000) { recreated.state.first { acknowledged.get() && !it.busy && it.session?.pendingInput == null } }
            assertEquals(SessionState.WaitingInput, recreated.state.value.session?.state)
            assertEquals(2, turns.get())
            assertEquals(listOf("draft-1", "draft-1"), submissions.map { it.pendingInputId })
            assertTrue(submissions.all { it.input == "Keep this first prompt" && it.signatures.isNotEmpty() })
            recreated.disconnect()
            reopened.connect()
            withTimeout(5_000) { reopened.state.first { it.connection == StreamConnection.Live } }
            assertNull(reopened.state.value.session?.pendingInput)
            assertEquals(2, turns.get(), "Reopening an acknowledged session must not resubmit the prompt")
        } finally {
            release.countDown(); vm.disconnect(); recreated.disconnect(); reopened.disconnect(); client.close(); server.stop(0); executor.shutdownNow()
        }
    }
}
