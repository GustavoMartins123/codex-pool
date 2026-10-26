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

class RunnerCatalogStreamTest {
    @Test fun snapshotsUseBearerAndResumeWithTheirContentRevision() = runTest {
        val initial = RunnerCatalogSnapshot("a".repeat(64), emptyList(), emptyList())
        val next = initial.copy(revision = "b".repeat(64))
        val seen = CopyOnWriteArrayList<Pair<String?, String?>>()
        val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/api/me/runner-catalog/events") { exchange ->
            seen += exchange.requestHeaders.getFirst("Authorization") to exchange.requestHeaders.getFirst("Last-Event-ID")
            exchange.responseHeaders.set("Content-Type", "text/event-stream")
            exchange.sendResponseHeaders(200, 0)
            exchange.responseBody.use { body ->
                for (snapshot in listOf(initial, next)) {
                    body.write("id: ${snapshot.revision}\nevent: catalog.snapshot\ndata: ${PoolJson.encodeToString(snapshot)}\n\n".toByteArray())
                    body.flush()
                }
            }
        }
        server.start()
        val client = HttpClient(CIO) { poolConfiguration() }
        client.plugin(HttpSend).intercept { request ->
            request.url.protocol = URLProtocol.HTTP; request.url.host = "127.0.0.1"; request.url.port = server.address.port
            execute(request)
        }
        try {
            val api = PoolApi(client, Credentials("https://pool.example", "app-token", "client"))
            assertEquals(listOf(initial, next), api.runnerCatalogEvents("c".repeat(64)).toList())
            assertEquals(listOf("Bearer app-token" to "c".repeat(64)), seen.toList())
        } finally { client.close(); server.stop(0) }
    }
}
