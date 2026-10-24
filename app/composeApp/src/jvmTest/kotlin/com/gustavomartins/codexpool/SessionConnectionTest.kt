@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)

package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.gustavomartins.codexpool.ui.*
import com.sun.net.httpserver.HttpExchange
import com.sun.net.httpserver.HttpServer
import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.HttpSend
import io.ktor.client.plugins.plugin
import io.ktor.http.URLProtocol
import java.net.InetSocketAddress
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import kotlin.test.*

class SessionConnectionTest {
    @BeforeTest fun mainDispatcher() { Dispatchers.setMain(Dispatchers.Unconfined) }
    @AfterTest fun resetDispatcher() { Dispatchers.resetMain() }

    @Test fun deltasArriveBeforeCompletionAndReconnectUsesLastCursorEvenWhenSessionClosed() = runBlocking {
        val continueFirst = CountDownLatch(1)
        val closed = AtomicBoolean(false)
        val cursors = CopyOnWriteArrayList<String?>()
        Fixture(closed) { exchange ->
            val cursor = exchange.requestHeaders.getFirst("Last-Event-ID")
            cursors += cursor
            exchange.responseHeaders.set("Content-Type", "text/event-stream")
            exchange.sendResponseHeaders(200, 0)
            exchange.responseBody.bufferedWriter().use { out ->
                if (cursor == "0") {
                    out.write("id: 1\nevent: turn.delta\ndata: {\"turn_id\":\"t\",\"kind\":\"text\",\"content\":\"hello\"}\n\n")
                    out.flush()
                    check(continueFirst.await(5, TimeUnit.SECONDS))
                    // The connection drops before the remaining events can reach the app.
                    closed.set(true)
                } else {
                    out.write("id: 2\nevent: turn.delta\ndata: {\"turn_id\":\"t\",\"kind\":\"text\",\"content\":\" world\"}\n\n")
                    out.write("id: 3\nevent: turn.finished\ndata: {\"turn_id\":\"t\",\"tokens\":12,\"duration_ms\":340}\n\n")
                    out.write("id: 4\nevent: session.closed\ndata: {\"reason\":\"user\"}\n\n")
                }
            }
        }.use { fixture ->
            val vm = fixture.viewModel()
            try {
                vm.connect()
                val partial = withTimeout(5_000) { vm.state.first { it.entries.firstOrNull()?.content == "hello" } }
                assertEquals(StreamConnection.Live, partial.connection)
                assertTrue(partial.entries.none { it.kind == "summary" })
                continueFirst.countDown()
                val completed = withTimeout(5_000) { vm.state.first { it.entries.size == 2 && it.connection == StreamConnection.Disconnected } }
                assertEquals("hello world", completed.entries.first().content)
                assertEquals(SessionState.Closed, completed.session?.state)
                assertEquals(listOf("0", "1"), cursors.toList())
                assertEquals(1, completed.entries.count { it.kind == "summary" })
            } finally { continueFirst.countDown(); vm.disconnect() }
        }
    }

    @Test fun authAndExpiredCursorErrorsStopWithoutChangingTransportOrRetrying() = runBlocking {
        for (status in listOf(401, 403, 409)) {
            Fixture { exchange -> exchange.sendResponseHeaders(status, -1); exchange.close() }.use { fixture ->
                val vm = fixture.viewModel()
                try {
                    vm.connect()
                    val state = withTimeout(5_000) { vm.state.first { it.connection == StreamConnection.Disconnected && it.error != null } }
                    assertFalse(state.canSend)
                    if (status == 409) assertTrue(state.error!!.contains("cursor"))
                    delay(650)
                    assertEquals(1, fixture.streams.size, "HTTP $status must not reconnect")
                } finally { vm.disconnect() }
            }
        }
    }

    @Test fun unknownEventAndServerErrorStopWithAnExplicitError() = runBlocking {
        for (event in listOf("event: unknown.protocol\ndata: {}\n\n", "event: error\ndata: {\"message\":\"event cursor lost\"}\n\n")) {
            Fixture { exchange ->
                exchange.responseHeaders.set("Content-Type", "text/event-stream")
                exchange.sendResponseHeaders(200, 0)
                exchange.responseBody.use { it.write(event.toByteArray()) }
            }.use { fixture ->
                val vm = fixture.viewModel()
                try {
                    vm.connect()
                    val state = withTimeout(5_000) { vm.state.first { it.connection == StreamConnection.Disconnected && it.error != null } }
                    assertNotNull(state.error)
                    delay(650)
                    assertEquals(1, fixture.streams.size)
                } finally { vm.disconnect() }
            }
        }
    }

    private class Fixture(
        private val closed: AtomicBoolean = AtomicBoolean(false),
        stream: (HttpExchange) -> Unit,
    ) : AutoCloseable {
        val streams = CopyOnWriteArrayList<String?>()
        private val executor = Executors.newCachedThreadPool()
        private val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0).apply {
            this.executor = this@Fixture.executor
            createContext("/api/") { exchange ->
                check(exchange.requestHeaders.getFirst("Authorization") == "Bearer synthetic-token")
                if (exchange.requestURI.path.endsWith("/events")) {
                    streams += exchange.requestHeaders.getFirst("Last-Event-ID")
                    stream(exchange)
                } else {
                    val state = if (closed.get()) "closed" else "running"
                    val session = """{"id":"s","conversation_id":"s","agent":"codex","model":"synthetic","workspace":{"mode":"ephemeral"},"permission_mode":"gated","state":"$state","approvals":[]}"""
                    val conversation = """{"id":"s","session_id":"s","agent":"codex","model":"synthetic","title":"","storage":"active","session_state":"$state","last_interaction_at":null,"preview":[],"resume_available":false,"deleting":false}"""
                    val data = (if (exchange.requestURI.path.endsWith("/history")) {
                        """{"conversation":$conversation,"session":$session,"events":[],"before":1,"through":0,"has_older":false}"""
                    } else if (exchange.requestURI.path == "/api/conversations/s") conversation else session).toByteArray()
                    exchange.responseHeaders.set("Content-Type", "application/json")
                    exchange.sendResponseHeaders(200, data.size.toLong())
                    exchange.responseBody.use { it.write(data) }
                }
            }
            start()
        }
        private val client = HttpClient(OkHttp) { poolConfiguration() }.apply {
            plugin(HttpSend).intercept { request ->
                request.url.protocol = URLProtocol.HTTP
                request.url.host = "127.0.0.1"
                request.url.port = server.address.port
                execute(request)
            }
        }
        fun viewModel() = SessionViewModel(PoolApi(client, Credentials("https://pool.example", "synthetic-token", "client")), "s")
        override fun close() { client.close(); server.stop(0); executor.shutdownNow() }
    }
}
