package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.sun.net.httpserver.HttpServer
import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.HttpSend
import io.ktor.client.plugins.plugin
import io.ktor.http.URLProtocol
import java.net.InetSocketAddress
import java.util.concurrent.CopyOnWriteArrayList
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.test.runTest
import kotlin.test.*

class PoolSseTest {
    @Test fun okhttpKeepsTheStreamOpenWhileWaitingForTheServerHeartbeat() = runTest {
        val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/api/conversations/s-1/events") { exchange ->
            exchange.responseHeaders.set("Content-Type", "text/event-stream")
            exchange.sendResponseHeaders(200, 0)
            exchange.responseBody.use { body ->
                body.write("id: 1\nevent: state\ndata: {\"state\":\"waiting_input\"}\n\n".toByteArray())
                body.flush()
                // OkHttp's default ten-second read timeout is shorter than the
                // broker's fifteen-second heartbeat interval.
                Thread.sleep(12_000)
                body.write("id: 2\nevent: session.closed\ndata: {\"reason\":\"user\"}\n\n".toByteArray())
            }
        }
        server.start()
        val client = HttpClient(OkHttp) { poolConfiguration() }
        client.plugin(HttpSend).intercept { request ->
            request.url.protocol = URLProtocol.HTTP
            request.url.host = "127.0.0.1"
            request.url.port = server.address.port
            execute(request)
        }
        try {
            val events = PoolApi(client, Credentials("https://pool.example", "test-token", "app-client"))
                .events("s-1") {}.toList().map { it.event }
            assertEquals(2, events.size)
            assertEquals(SessionEvent.Closed(ClosedEvent("user")), events.last())
        } finally { client.close(); server.stop(0) }
    }

    @Test fun sseUsesBearerAndParsesMultilineEventsOnARealConnection() = runTest {
        val received = CopyOnWriteArrayList<Pair<String, String?>>()
        val cursors = CopyOnWriteArrayList<String?>()
        val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/api/conversations/s-1/events") { exchange ->
            received += exchange.requestURI.path to exchange.requestHeaders.getFirst("Authorization")
            cursors += exchange.requestHeaders.getFirst("Last-Event-ID")
            exchange.responseHeaders.set("Content-Type", "text/event-stream")
            exchange.sendResponseHeaders(200, 0)
            exchange.responseBody.use { body ->
                if (exchange.requestHeaders.getFirst("Last-Event-ID") == "2") {
                    body.write("id: 3\nevent: session.closed\ndata: {\"reason\":\"user\"}\n\n".toByteArray())
                    return@use
                }
                body.write(": heartbeat\n\nid: 1\nevent: turn.delta\ndata: {\"turn_id\":\"t-1\",\n".toByteArray())
                body.flush()
                body.write("data: \"kind\":\"text\",\"content\":\"hello\"}\n\nid: 2\nevent: session.closed\ndata: {\"reason\":\"user\"}\n\n".toByteArray())
            }
        }
        server.start()
        val client = HttpClient(CIO) { poolConfiguration() }
        // Route the test origin to a local HTTP fixture; production requires HTTPS.
        client.plugin(HttpSend).intercept { request ->
            request.url.protocol = URLProtocol.HTTP
            request.url.host = "127.0.0.1"
            request.url.port = server.address.port
            execute(request)
        }
        try {
            var connected = false
            val api = PoolApi(client, Credentials("https://pool.example", "test-token", "app-client"))
            var cursor: String? = null
            val events = api.events("s-1", onCursor = { cursor = it }) { connected = true }.toList().map { it.event }
            assertTrue(connected)
            assertEquals(listOf("/api/conversations/s-1/events" to "Bearer test-token"), received.toList())
            assertEquals(listOf(SessionEvent.Delta(TurnDelta("t-1", DeltaKind.Text, "hello")), SessionEvent.Closed(ClosedEvent("user"))), events)
            assertEquals("2", cursor)
            val resumed = api.events("s-1", cursor, onCursor = { cursor = it }) {}.toList().map { it.event }
            assertEquals(listOf(SessionEvent.Closed(ClosedEvent("user"))), resumed)
            assertEquals(listOf(null, "2"), cursors.toList())
            assertEquals("3", cursor)
        } finally { client.close(); server.stop(0) }
    }
}
