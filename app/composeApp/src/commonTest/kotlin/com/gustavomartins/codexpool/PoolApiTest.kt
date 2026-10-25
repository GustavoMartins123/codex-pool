package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import io.ktor.http.content.TextContent
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.jsonObject
import kotlin.test.*

class PoolApiTest {
    private val credentials = Credentials("https://pool.example", "test-session-token", "app-client")
    private val session = Session("s-1", Agent.Codex, "model-1", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated, SessionState.WaitingInput, conversationId = "s-1")
    private val jsonHeaders = headersOf(HttpHeaders.ContentType, "application/json")

    @Test fun loginUsesDedicatedTokenEndpointWithoutBearer() = runTest {
        val engine = MockEngine { request ->
            assertEquals("https://pool.example/api/auth/token", request.url.toString())
            assertEquals(HttpMethod.Post, request.method)
            assertNull(request.headers[HttpHeaders.Authorization])
            val body = PoolJson.decodeFromString<LoginRequest>((request.body as TextContent).text)
            assertEquals(LoginRequest("user", "password"), body)
            val keys = PoolJson.parseToJsonElement((request.body as TextContent).text).jsonObject.keys
            assertEquals(setOf("email", "password"), keys)
            respond("""{"token":"test-token","client_id":"mobile"}""", HttpStatusCode.OK, jsonHeaders)
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try { assertEquals(Credentials("https://pool.example", "test-token", "mobile"), PoolApi.login(client, "https://pool.example/", "user", "password")) }
        finally { client.close() }
    }

    @Test fun missingCanonicalEndpointFailsWithoutAlternativeRequest() = runTest {
        val engine = MockEngine { respond("missing", HttpStatusCode.NotFound) }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val error = assertFailsWith<PoolApiException> { PoolApi.login(client, credentials.baseUrl, "user", "password") }
            assertEquals(404, error.status)
            assertEquals(1, engine.requestHistory.size)
            assertEquals("/api/auth/token", engine.requestHistory.single().url.encodedPath)
        } finally { client.close() }
    }

    @Test fun redirectIsRejectedWithoutSendingCredentialsToAnotherHost() = runTest {
        val engine = MockEngine { respond("", HttpStatusCode.TemporaryRedirect, headersOf(HttpHeaders.Location, "https://other.example/login")) }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            assertEquals(307, assertFailsWith<PoolApiException> { PoolApi.login(client, credentials.baseUrl, "user", "password") }.status)
            assertEquals(1, engine.requestHistory.size)
        } finally { client.close() }
    }

    @Test fun insecureOrAmbiguousPoolUrlsAreRejected() {
        listOf("http://pool.example", "https://user:password@pool.example", "https://pool.example/api", "https://pool.example/?token=secret", "https://pool.example/#fragment")
            .forEach { assertFailsWith<IllegalArgumentException>(it) { normalizePoolUrl(it) } }
    }

    @Test fun pathWorkspaceRequiresGatedBeforeSendingAnything() = runTest {
        val engine = MockEngine { error("No HTTP request is allowed") }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            assertFailsWith<IllegalArgumentException> { PoolApi(client, credentials).create(CreateSession(Agent.Codex, "model-1", Workspace(WorkspaceMode.Path, "/work"), PermissionMode.Auto)) }
            assertTrue(engine.requestHistory.isEmpty())
        } finally { client.close() }
    }

    @Test fun sessionCommandsUseTheSpecifiedMethodsAndBearer() = runTest {
        val engine = MockEngine { request ->
            assertEquals("Bearer test-session-token", request.headers[HttpHeaders.Authorization])
            when (request.url.encodedPath) {
                "/api/sessions" -> {
                    assertEquals(HttpMethod.Post, request.method)
                    val body = PoolJson.decodeFromString<CreateSession>((request.body as TextContent).text)
                    assertEquals(PermissionMode.Gated, body.permissionMode)
                    respond(PoolJson.encodeToString(session), HttpStatusCode.Created, jsonHeaders)
                }
                "/api/sessions/s-1/turns" -> {
                    assertEquals(HttpMethod.Post, request.method)
                    assertEquals(TurnInput("continue"), PoolJson.decodeFromString<TurnInput>((request.body as TextContent).text))
                    respond("""{"turn_id":"t-1"}""", HttpStatusCode.Created, jsonHeaders)
                }
                "/api/sessions/s-1/turns:cancel" -> { assertEquals(HttpMethod.Post, request.method); respond("", HttpStatusCode.NoContent) }
                "/api/sessions/s-1/approvals/a-1" -> {
                    assertEquals(HttpMethod.Post, request.method)
                    assertEquals(ApprovalDecision(Decision.Deny), PoolJson.decodeFromString<ApprovalDecision>((request.body as TextContent).text))
                    respond("", HttpStatusCode.NoContent)
                }
                "/api/sessions/s-1" -> { assertEquals(HttpMethod.Delete, request.method); respond("", HttpStatusCode.NoContent) }
                else -> error("Unexpected request: ${request.url}")
            }
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val api = PoolApi(client, credentials)
            assertEquals(session, api.create(CreateSession(Agent.Codex, "model-1", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated)))
            assertEquals("t-1", api.turn("s-1", "continue").turnId)
            api.cancel("s-1"); api.decide("s-1", "a-1", Decision.Deny); api.close("s-1")
            assertEquals(5, engine.requestHistory.size)
        } finally { client.close() }
    }

    @Test fun signedCommandsCarryIssueTimeAndSignatures() = runTest {
        val bodies = mutableListOf<String>()
        val engine = MockEngine { request ->
            bodies += (request.body as TextContent).text
            if (request.url.encodedPath.endsWith("/turns")) respond("""{"turn_id":"t-1"}""", HttpStatusCode.Created, jsonHeaders)
            else respond("", HttpStatusCode.NoContent)
        }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val api = PoolApi(client, credentials)
            val signed = SignedCommand(1761379200000L, mapOf("CYxfwqA9-ufHeg_CwDnJkw" to "x".repeat(43)))
            api.turn("s-1", "continue", signed = signed)
            api.decide("s-1", "a-1", Decision.Approve, signed)
            assertEquals(TurnInput("continue", issuedAt = signed.issuedAt, signatures = signed.signatures), PoolJson.decodeFromString<TurnInput>(bodies[0]))
            assertEquals(ApprovalDecision(Decision.Approve, signed.issuedAt, signed.signatures), PoolJson.decodeFromString<ApprovalDecision>(bodies[1]))
        } finally { client.close() }
    }

    @Test fun invalidResourceIdsAndEmptyTurnsNeverReachTheNetwork() = runTest {
        val engine = MockEngine { error("No HTTP request is allowed") }
        val client = HttpClient(engine) { poolConfiguration() }
        try {
            val api = PoolApi(client, credentials)
            assertFailsWith<IllegalArgumentException> { api.session("../foreign?token=x") }
            assertFailsWith<IllegalArgumentException> { api.turn("s-1", "   ") }
            assertTrue(engine.requestHistory.isEmpty())
        } finally { client.close() }
    }

    @Test fun malformedAndUnknownEventsFailExplicitly() {
        assertFails { decodeEvent("new.protocol", "{}") }
        assertFails { decodeEvent("turn.delta", """{"turn_id":"t-1","kind":"unknown","content":"x"}""") }
        assertFails { decodeEvent("approval.requested", "{}") }
    }

    @Test fun catalogAndQuotasDecodeExistingBackendProjections() {
        val catalog = PoolJson.decodeFromString<Catalog>("""{"schema_version":1,"models":[{"id":"m","name":"Model","protocol":"openai","available_now":true,"provider":"codex","contextWindow":200000}]}""")
        assertEquals("m", catalog.models.single().id)
        val quotas = PoolJson.decodeFromString<Quotas>("""{"generated_at":"2026-10-23T00:00:00Z","models":[{"model":"m","name":"Model","provider":"codex","supporting_accounts":1,"available_now":false,"primary":null,"secondary":null}]}""")
        assertNull(quotas.models.single().primary)
        assertFalse(quotas.models.single().availableNow)
    }
}
