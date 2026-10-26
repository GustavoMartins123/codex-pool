package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.*
import io.ktor.http.*
import io.ktor.http.content.TextContent
import kotlinx.coroutines.test.runTest
import kotlinx.io.IOException
import kotlin.test.*

class BootstrapCoordinatorTest {
    private val origin = "https://pool.example"
    private val pool = origin
    private val secret = "c".repeat(42) + "A"
    private val payload = BootstrapPayload(1, "b".repeat(42) + "A", listOf("https://192.168.1.20:49152"), "p".repeat(42) + "A", secret, "2026-10-26T12:05:00Z")
    private val commandKey = base64UrlEncode(ByteArray(32) { 9 })
    private val commandKeyId = CommandSigning.keyId(CommandSigning.decodeKey(commandKey))
    private val pairingId = "r".repeat(43)
    private val runnerId = "11111111-2222-3333-4444-555555555555"
    private val headers = headersOf(HttpHeaders.ContentType, "application/json")

    private inner class Harness(
        var ticketPool: String = origin,
        var reviewRunner: String? = null,
        var lanAfterApproval: Boolean = true,
        var storeFails: Boolean = false,
        var listedRunners: List<Runner>? = null,
    ) {
        val events = mutableListOf<String>()
        var approved = false
        var operation = ""

        val local = HttpClient(MockEngine { request ->
            check(request.headers[RunnerContract.BootstrapSecretHeader] == secret) { "local call without the QR secret" }
            check(request.headers[HttpHeaders.Authorization] == null) { "pool bearer sent to the runner" }
            val path = request.url.encodedPath.removePrefix(RunnerContract.BootstrapPathPrefix)
            if (approved && !lanAfterApproval) throw IOException("LAN unreachable")
            when (path) {
                "/begin" -> {
                    val begin = PoolJson.decodeFromString<BootstrapBegin>((request.body as TextContent).text)
                    events += "begin ${begin.poolUrl}"
                    operation = begin.operationId
                    respond(PoolJson.encodeToString(BootstrapTicket(payload.bootstrapId, begin.operationId, ticketPool, pairingId, "AbCd1234",
                        "desktop", "linux", "amd64", commandKey, CommandSigning.keyId(CommandSigning.decodeKey(commandKey)), "2026-10-26T12:10:00Z")), HttpStatusCode.OK, headers)
                }
                "/status" -> {
                    val phase = if (approved) BootstrapPhase.Paired else BootstrapPhase.AwaitingApproval
                    respond(PoolJson.encodeToString(BootstrapStatus(payload.bootstrapId, phase, runnerId = runnerId.takeIf { approved },
                        commandKeyId = CommandSigning.keyId(CommandSigning.decodeKey(commandKey)))), HttpStatusCode.OK, headers)
                }
                "/ack", "/cancel" -> { events += path.removePrefix("/"); respond("", HttpStatusCode.NoContent) }
                else -> error("unexpected local path $path")
            }
        })

        val api = PoolApi(HttpClient(MockEngine { request ->
            check(request.headers[RunnerContract.BootstrapSecretHeader] == null) { "QR secret sent to the pool" }
            when (request.url.encodedPath) {
                "/api/me/runner-pairings/$pairingId" -> if (request.method == HttpMethod.Get) {
                    respond(PoolJson.encodeToString(PairingReview(pairingId, "desktop", "linux", "amd64", reviewRunner, "2026-10-26T12:10:00Z")), HttpStatusCode.OK, headers)
                } else {
                    val decision = PoolJson.decodeFromString<PairingDecision>((request.body as TextContent).text)
                    events += "decide ${decision.decision.wire} ${decision.userCode}"
                    approved = decision.decision == Decision.Approve
                    respond("", HttpStatusCode.NoContent)
                }
                "/api/me/runners" -> respond(PoolJson.encodeToString(RunnerList(listedRunners ?: listOf(
                    Runner(runnerId, "desktop", online = approved, agents = listOf(Agent.Codex), commandKeyId = commandKeyId)
                ))), HttpStatusCode.OK, headers)
                else -> error("unexpected pool path ${request.url.encodedPath}")
            }
        }) { poolConfiguration() }, Credentials(pool, "token", "client"))

        val coordinator = BootstrapCoordinator(api, { BootstrapApi(local, payload, payload.endpoints.single()) },
            rememberKey = { key -> if (storeFails) error("disk full"); events += "store $key" }, pause = {})
    }

    @Test fun storesTheKeyBeforeApprovingAndWaitsForThePool() = runTest {
        val h = Harness()
        h.coordinator.prepare()
        assertEquals(BootstrapStep.Confirm, h.coordinator.progress.value.step)
        h.coordinator.approve()
        assertEquals(BootstrapStep.Online, h.coordinator.progress.value.step)
        assertEquals(runnerId, h.coordinator.progress.value.runnerId)
        assertEquals(listOf("begin $origin", "store $commandKey", "decide approve AbCd1234", "ack"), h.events)
    }

    @Test fun neverApprovesWhenTheKeyCannotBeStored() = runTest {
        val h = Harness(storeFails = true)
        h.coordinator.prepare()
        assertFails { h.coordinator.approve() }
        assertEquals(BootstrapStep.Failed, h.coordinator.progress.value.step)
        assertTrue(h.events.none { it.startsWith("decide") })
    }

    @Test fun refusesTicketsForAnotherPoolOrMachine() = runTest {
        for (h in listOf(Harness(ticketPool = "https://evil.example"), Harness(reviewRunner = runnerId))) {
            assertFails { h.coordinator.prepare() }
            assertEquals(BootstrapStep.Failed, h.coordinator.progress.value.step)
            assertFails { h.coordinator.approve() }
            assertTrue(h.events.none { it.startsWith("decide") || it.startsWith("store") })
        }
    }

    @Test fun confirmsThroughThePoolWhenTheLanDropsAfterApproval() = runTest {
        val h = Harness(lanAfterApproval = false)
        h.coordinator.prepare()
        h.coordinator.approve()
        assertEquals(BootstrapStep.Online, h.coordinator.progress.value.step)
        assertEquals(runnerId, h.coordinator.progress.value.runnerId)
    }

    @Test fun neverConfirmsAnotherRunnerWithTheSameName() = runTest {
        val otherKeyId = CommandSigning.keyId(ByteArray(32) { 10 })
        for (keyId in listOf(otherKeyId, null)) {
            val h = Harness(lanAfterApproval = false, listedRunners = listOf(
                Runner("another-runner", "desktop", online = true, agents = listOf(Agent.Codex), commandKeyId = keyId)
            ))
            h.coordinator.prepare()
            h.coordinator.approve()
            assertEquals(BootstrapStep.WaitingOnline, h.coordinator.progress.value.step)
            assertNull(h.coordinator.progress.value.runnerId)
        }
    }

    @Test fun confirmsTheKeyOwnerEvenWhenNamesCollideOrChange() = runTest {
        val h = Harness(lanAfterApproval = false, listedRunners = listOf(
            Runner("another-runner", "desktop", online = true, agents = listOf(Agent.Codex)),
            Runner(runnerId, "renamed", online = true, agents = listOf(Agent.Codex), commandKeyId = commandKeyId)
        ))
        h.coordinator.prepare()
        h.coordinator.approve()
        assertEquals(BootstrapStep.Online, h.coordinator.progress.value.step)
        assertEquals(runnerId, h.coordinator.progress.value.runnerId)
    }

    @Test fun requiresTheMatchingRunnerToBeOnlineAndUsable() = runTest {
        for (runner in listOf(
            Runner(runnerId, "desktop", online = false, agents = emptyList(), commandKeyId = commandKeyId),
            Runner(runnerId, "desktop", online = true, agents = emptyList(), revoked = true, commandKeyId = commandKeyId),
        )) {
            val h = Harness(lanAfterApproval = false, listedRunners = listOf(runner))
            h.coordinator.prepare()
            h.coordinator.approve()
            assertEquals(BootstrapStep.WaitingOnline, h.coordinator.progress.value.step)
        }
    }

    @Test fun preservesTheRunnerIdConfirmedOverTheLan() = runTest {
        val h = Harness(listedRunners = listOf(
            Runner("another-runner", "desktop", online = true, agents = listOf(Agent.Codex), commandKeyId = commandKeyId)
        ))
        h.coordinator.prepare()
        h.coordinator.approve()
        assertEquals(BootstrapStep.WaitingOnline, h.coordinator.progress.value.step)
        assertEquals(runnerId, h.coordinator.progress.value.runnerId)
    }

    @Test fun cancellingBeforeApprovalTellsTheRunner() = runTest {
        val h = Harness()
        h.coordinator.prepare()
        h.coordinator.cancel()
        assertEquals("cancel", h.events.last())
    }

    @Test fun keysGoToTheAccountThatApproved() {
        val a = Credentials("https://a.example", "ta", "ca")
        val b = Credentials("https://b.example", "tb", "cb")
        val accounts = PoolAccounts(selectedUrl = b.baseUrl, providers = listOf(PoolProvider("A", a.baseUrl, a), PoolProvider("B", b.baseUrl, b)))
        val stored = accounts.withCommandKey(commandKey, a)
        assertEquals(listOf(commandKey), stored.providers.first { it.baseUrl == a.baseUrl }.commandKeys)
        assertTrue(stored.selected.commandKeys.isEmpty())
        assertFailsWith<IllegalArgumentException> { accounts.withCommandKey(commandKey, a.copy(clientId = "other")) }
        assertFailsWith<IllegalArgumentException> { accounts.withCommandKey(commandKey, Credentials("https://c.example", "t", "c")) }
    }

    @Test fun originsAndPinsMatchTheRunner() {
        assertEquals("https://pool.example", normalizeOrigin("https://Pool.Example:443"))
        assertEquals("https://pool.example:8443", normalizeOrigin("https://pool.example:8443"))
        assertEquals("https://[::1]", normalizeOrigin("https://[::1]"))
        val spki = ByteArray(91) { it.toByte() }
        assertTrue(bootstrapPinMatches(spki, base64UrlEncode(sha256(spki))))
        assertFalse(bootstrapPinMatches(spki, base64UrlEncode(sha256(spki + 1))))
        assertTrue(hasDuplicateKeys("""{"a":1,"b":[{"a":2}],"a":3}"""))
        assertFalse(hasDuplicateKeys("""{"a":"x,\"a\":","b":[{"a":2}]}"""))
    }
}
