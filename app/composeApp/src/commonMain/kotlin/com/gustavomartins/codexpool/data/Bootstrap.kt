package com.gustavomartins.codexpool.data

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlin.random.Random
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds
import kotlin.time.Instant

/** What the scanner read: a legacy pairing for this pool, or a runner waiting on the LAN. */
sealed interface RunnerQr {
    data class Pairing(val qr: PairingQr) : RunnerQr
    data class Bootstrap(val payload: BootstrapPayload) : RunnerQr
}

private const val BOOTSTRAP_PREFIX = "codexpool://bootstrap?"

fun parseRunnerQr(link: String, pool: String, now: Instant): RunnerQr =
    if (link.startsWith("codexpool://bootstrap")) RunnerQr.Bootstrap(parseBootstrapLink(link, now))
    else RunnerQr.Pairing(parsePairingLink(link, pool))

private val strictJson = Json { ignoreUnknownKeys = false; isLenient = false }

/**
 * Accepts only the QR runnerd renders (internal/runnerproto/bootstrap.go); both
 * parsers run contracts/fixtures/bootstrap_links.json. It names no pool: the app
 * supplies its own account's origin, so a QR can never redirect the login.
 */
fun parseBootstrapLink(link: String, now: Instant?): BootstrapPayload {
    val invalid = "QR do runner inválido."
    require(link.length <= RunnerContract.BootstrapMaxLinkBytes && link.startsWith(BOOTSTRAP_PREFIX) &&
        link.none { it.code < 0x21 || it.code == 0x7f } && link.none { it in "#%+;" }) { invalid }
    val params = link.removePrefix(BOOTSTRAP_PREFIX).split('&').map {
        val parts = it.split('=')
        require(parts.size == 2) { invalid }
        parts[0] to parts[1]
    }
    require(params.map { it.first }.sorted() == listOf("payload", "v") &&
        params.toMap()["v"] == RunnerContract.BootstrapVersion.toString()) { invalid }
    val raw = runCatching { base64UrlDecode(params.toMap().getValue("payload")).decodeToString(throwOnInvalidSequence = true) }.getOrNull()
    // runnerd never escapes anything in this payload; escapes would let the
    // two parsers decode keys differently.
    require(raw != null && '\\' !in raw && !hasDuplicateKeys(raw)) { invalid }
    val payload = runCatching {
        // kotlinx.serialization accepts "1" for an Int; encoding/json does not.
        val fields = strictJson.parseToJsonElement(raw) as JsonObject
        require(fields.all { (name, value) ->
            when (name) {
                "version" -> value is JsonPrimitive && !value.isString
                "endpoints" -> value is JsonArray && value.all { it is JsonPrimitive && it.isString }
                else -> value is JsonPrimitive && value.isString
            }
        })
        strictJson.decodeFromJsonElement(BootstrapPayload.serializer(), fields)
    }.getOrNull()
    require(payload != null && payload.version == RunnerContract.BootstrapVersion &&
        listOf(payload.bootstrapId, payload.bootstrapSecret, payload.tlsSpkiSha256).all(::isToken32) &&
        payload.endpoints.size in 1..RunnerContract.BootstrapMaxEndpoints && payload.endpoints.distinct().size == payload.endpoints.size &&
        payload.endpoints.all { bootstrapEndpointHost(it) != null }) { invalid }
    val expires = runCatching { Instant.parse(payload.expiresAt) }.getOrNull()
    require(expires != null) { invalid }
    if (now != null) {
        require(expires > now && expires <= now + RunnerContract.BootstrapTtlSeconds.seconds + 10.minutes) {
            "O QR do runner expirou. Use o QR novo exibido no computador."
        }
    }
    return payload
}

private val endpointPattern = Regex("""https://(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3}):(\d{1,5})""")

/** The IPv4 host of a canonical https://<private IPv4>:<port> endpoint, else null. */
fun bootstrapEndpointHost(endpoint: String): String? {
    val match = endpointPattern.matchEntire(endpoint) ?: return null
    val parts = match.groupValues.drop(1)
    val numbers = parts.map { it.toIntOrNull() ?: return null }
    if (parts.zip(numbers).any { (text, n) -> text != n.toString() }) return null
    val (a, b, c, d, port) = numbers
    if (listOf(a, b, c, d).any { it > 255 } || port !in 1024..65535) return null
    val private = a == 10 || a == 172 && b in 16..31 || a == 192 && b == 168
    return if (private) "$a.$b.$c.$d" else null
}

private fun isToken32(value: String) = value.length == 43 && runCatching { base64UrlDecode(value).size == 32 }.getOrDefault(false)

/** kotlinx.serialization keeps the last of repeated keys; refuse them instead. */
internal fun hasDuplicateKeys(json: String): Boolean {
    val stack = ArrayDeque<MutableSet<String>?>()
    var i = 0
    var expectKey = false
    while (i < json.length) {
        when (val c = json[i]) {
            '{' -> { stack.addLast(mutableSetOf()); expectKey = true }
            '[' -> { stack.addLast(null); expectKey = false }
            '}', ']' -> { stack.removeLastOrNull(); expectKey = false }
            ',' -> expectKey = stack.lastOrNull() != null
            '"' -> {
                val start = ++i
                while (i < json.length && json[i] != '"') i += if (json[i] == '\\') 2 else 1
                if (expectKey && stack.lastOrNull()?.add(json.substring(start, minOf(i, json.length))) == false) return true
                expectKey = false
            }
            else -> if (c == ':') expectKey = false
        }
        i++
    }
    return false
}

/** The spelling runnerd stores for a pool origin: lowercase host, no default port. */
fun normalizeOrigin(origin: String): String {
    val rest = origin.removePrefix("https://")
    val colon = rest.lastIndexOf(':')
    val (host, port) = if (colon > rest.lastIndexOf(']')) rest.substring(0, colon) to rest.substring(colon + 1) else rest to ""
    return "https://" + host.lowercase() + if (port.isEmpty() || port == "443") "" else ":$port"
}

/** Idempotency key for one onboarding; it authorizes nothing by itself. */
fun newBootstrapOperationId(random: Random = Random.Default): String = base64UrlEncode(random.nextBytes(32))

/** The SPKI pin in the QR: SHA-256 of the certificate's SubjectPublicKeyInfo. */
fun bootstrapPinMatches(subjectPublicKeyInfo: ByteArray, pin: String): Boolean {
    val actual = base64UrlEncode(sha256(subjectPublicKeyInfo))
    if (actual.length != pin.length) return false
    var diff = 0
    for (i in actual.indices) diff = diff or (actual[i].code xor pin[i].code)
    return diff == 0
}
