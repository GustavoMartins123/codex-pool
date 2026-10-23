package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import com.sun.net.httpserver.HttpServer
import io.ktor.client.HttpClient
import io.ktor.client.engine.cio.CIO
import io.ktor.client.plugins.HttpSend
import io.ktor.client.plugins.plugin
import io.ktor.http.URLProtocol
import java.net.InetSocketAddress
import java.util.concurrent.CopyOnWriteArrayList
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.test.runTest
import kotlin.test.*

class PoolSseTest {
    @Test fun sseUsesBearerAndParsesMultilineEventsOnARealConnection() = runTest {
        val received = CopyOnWriteArrayList<Pair<String, String?>>()
        val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/api/sessions/s-1/events") { exchange ->
            received += exchange.requestURI.path to exchange.requestHeaders.getFirst("Authorization")
            exchange.responseHeaders.set("Content-Type", "text/event-stream")
            exchange.sendResponseHeaders(200, 0)
            exchange.responseBody.use { body ->
                body.write(": heartbeat\n\nevent: turn.delta\ndata: {\"turn_id\":\"t-1\",\n".toByteArray())
                body.flush()
                body.write("data: \"kind\":\"text\",\"content\":\"hello\"}\n\nevent: session.closed\ndata: {\"reason\":\"user\"}\n\n".toByteArray())
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
            val events = api.events("s-1") { connected = true }.toList()
            assertTrue(connected)
            assertEquals(listOf("/api/sessions/s-1/events" to "Bearer test-token"), received.toList())
            assertEquals(listOf(SessionEvent.Delta(TurnDelta("t-1", DeltaKind.Text, "hello")), SessionEvent.Closed(ClosedEvent("user"))), events)
        } finally { client.close(); server.stop(0) }
    }
}
