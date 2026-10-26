package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import kotlinx.serialization.json.*
import java.io.File
import kotlin.test.*
import kotlin.time.Instant

// Same fixtures as internal/runnerproto/bootstrap_test.go: both parsers must
// accept and refuse exactly the same links.
class BootstrapLinkTest {
    private val fixtures: JsonObject by lazy {
        val file = generateSequence(File(System.getProperty("user.dir")).absoluteFile) { it.parentFile }
            .map { File(it, "contracts/fixtures/bootstrap_links.json") }.first { it.isFile }
        Json.parseToJsonElement(file.readText()).jsonObject
    }
    private val now get() = Instant.parse(fixtures.getValue("now").jsonPrimitive.content)

    @Test fun acceptsTheValidFixtures() {
        for (case in fixtures.getValue("valid").jsonArray.map { it.jsonObject }) {
            val parsed = parseBootstrapLink(case.getValue("link").jsonPrimitive.content, now)
            assertEquals(Json.decodeFromJsonElement<BootstrapPayload>(case.getValue("payload")), parsed, case.getValue("name").jsonPrimitive.content)
        }
    }

    @Test fun refusesTheInvalidFixtures() {
        for (case in fixtures.getValue("invalid").jsonArray.map { it.jsonObject }) {
            val name = case.getValue("name").jsonPrimitive.content
            assertFailsWith<IllegalArgumentException>(name) { parseBootstrapLink(case.getValue("link").jsonPrimitive.content, now) }
        }
    }

    @Test fun legacyQrsStillNeedThisPool() {
        val valid = fixtures.getValue("valid").jsonArray.first().jsonObject.getValue("link").jsonPrimitive.content
        assertIs<RunnerQr.Bootstrap>(parseRunnerQr(valid, "https://pool.example", now))
        val legacy = "codexpool://pair?pool=https%3A%2F%2Fother.example&request=${"a".repeat(43)}"
        assertFailsWith<IllegalArgumentException> { parseRunnerQr(legacy, "https://pool.example", now) }
    }
}
