package com.gustavomartins.codexpool.data

import kotlin.time.Clock

/**
 * Signs runner commands with the key a runner shows in its pairing QR.
 * The key never reaches the pool, so the pool cannot forge prompts or
 * approvals for that runner. Mirrors internal/runnerproto/signing.go.
 */
object CommandSigning {
    fun decodeKey(value: String): ByteArray {
        val key = runCatching { base64UrlDecode(value) }.getOrNull()
        require(key != null && key.size == RunnerContract.CommandKeyBytes) { "Chave de comando do runner inválida." }
        return key
    }

    /** Names a key without revealing it. */
    fun keyId(key: ByteArray): String = base64UrlEncode(hmacSha256(key, RunnerContract.CommandKeyIDDomain.encodeToByteArray()).copyOf(RunnerContract.CommandKeyIDBytes))

    /** Binds a prompt to its session and to the settings that decide what the agent may do. */
    fun turnInput(sessionId: String, issuedAt: Long, agent: Agent, permissionMode: PermissionMode, workspace: Workspace, input: String): ByteArray =
        signingInput(RunnerContract.TurnSigningDomain, sessionId, issuedAt.toString(), agent.wire, permissionMode.wire, workspace.mode.wire, workspace.target.orEmpty(), input)

    /** Binds a decision to the exact command the user saw. */
    fun approvalInput(sessionId: String, approvalId: String, decision: Decision, issuedAt: Long, command: String): ByteArray =
        signingInput(RunnerContract.ApprovalSigningDomain, sessionId, approvalId, decision.wire, issuedAt.toString(), sha256(command.encodeToByteArray()).toHex())

    fun sign(key: ByteArray, input: ByteArray): String = base64UrlEncode(hmacSha256(key, input))

    // Length prefixes count UTF-8 bytes ("<bytes>:<field>;"), so no field can shift another.
    private fun signingInput(vararg fields: String): ByteArray {
        val out = ArrayList<Byte>()
        for (field in fields) {
            val bytes = field.encodeToByteArray()
            "${bytes.size}:".encodeToByteArray().forEach(out::add)
            bytes.forEach(out::add)
            out.add(';'.code.toByte())
        }
        return out.toByteArray()
    }
}

/** Commands carry the issue time and one signature per runner key this device holds. */
data class SignedCommand(val issuedAt: Long, val signatures: Map<String, String>)

class CommandSigner(
    private val keys: () -> List<String>,
    private val store: suspend (String) -> Unit,
    private val now: () -> Long = { Clock.System.now().toEpochMilliseconds() },
) {
    private var lastIssuedAt = 0L
    val enabled get() = keys().isNotEmpty()

    suspend fun remember(key: String) {
        CommandSigning.decodeKey(key)
        store(key)
    }

    fun signTurn(session: Session, input: String): SignedCommand = sign { issuedAt ->
        CommandSigning.turnInput(session.id, issuedAt, session.agent, session.permissionMode, session.workspace, input)
    }

    /** Denials stay unsigned: runners accept them from anyone because refusing is always safe. */
    fun signApproval(sessionId: String, approval: Approval, decision: Decision): SignedCommand? =
        if (decision == Decision.Deny) null else sign { issuedAt ->
            CommandSigning.approvalInput(sessionId, approval.approvalId, decision, issuedAt, approval.command)
        }

    // Runners reject a turn signature that is not newer than the last one they accepted.
    private fun sign(input: (Long) -> ByteArray): SignedCommand {
        val stored = keys()
        check(stored.isNotEmpty()) { MISSING_KEY }
        require(stored.size <= RunnerContract.MaxCommandSignatures && stored.distinct().size == stored.size) {
            "Conjunto de chaves de runner inválido: o limite por provedor é ${RunnerContract.MaxCommandSignatures}."
        }
        val issuedAt = maxOf(now(), lastIssuedAt + 1).also { lastIssuedAt = it }
        val message = input(issuedAt)
        return SignedCommand(issuedAt, stored.associate { encoded ->
            val key = CommandSigning.decodeKey(encoded)
            CommandSigning.keyId(key) to CommandSigning.sign(key, message)
        })
    }

    companion object {
        const val MISSING_KEY = "Este aparelho não tem a chave do runner. Rode runnerd rotate no computador e leia o novo QR."
        val Disabled = CommandSigner(keys = { emptyList() }, store = { error("Este aparelho não guarda chaves de runner.") })
    }
}

internal fun hmacSha256(key: ByteArray, message: ByteArray): ByteArray {
    val block = 64
    val normalized = (if (key.size > block) sha256(key) else key).copyOf(block)
    val inner = ByteArray(block) { (normalized[it].toInt() xor 0x36).toByte() }
    val outer = ByteArray(block) { (normalized[it].toInt() xor 0x5c).toByte() }
    return sha256(outer + sha256(inner + message))
}

private val sha256K = longArrayOf(
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
).map { it.toInt() }.toIntArray()

internal fun sha256(message: ByteArray): ByteArray {
    val h = longArrayOf(0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19).map { it.toInt() }.toIntArray()
    val bitLength = message.size.toLong() * 8
    val padded = message.copyOf(((message.size + 9 + 63) / 64) * 64)
    padded[message.size] = 0x80.toByte()
    for (i in 0 until 8) padded[padded.size - 1 - i] = (bitLength ushr (8 * i)).toByte()
    val w = IntArray(64)
    for (chunk in padded.indices step 64) {
        for (i in 0 until 16) {
            val j = chunk + i * 4
            w[i] = (padded[j].toInt() and 0xff shl 24) or (padded[j + 1].toInt() and 0xff shl 16) or
                (padded[j + 2].toInt() and 0xff shl 8) or (padded[j + 3].toInt() and 0xff)
        }
        for (i in 16 until 64) {
            val s0 = w[i - 15].rotateRight(7) xor w[i - 15].rotateRight(18) xor (w[i - 15] ushr 3)
            val s1 = w[i - 2].rotateRight(17) xor w[i - 2].rotateRight(19) xor (w[i - 2] ushr 10)
            w[i] = w[i - 16] + s0 + w[i - 7] + s1
        }
        var a = h[0]; var b = h[1]; var c = h[2]; var d = h[3]
        var e = h[4]; var f = h[5]; var g = h[6]; var hh = h[7]
        for (i in 0 until 64) {
            val t1 = hh + (e.rotateRight(6) xor e.rotateRight(11) xor e.rotateRight(25)) + ((e and f) xor (e.inv() and g)) + sha256K[i] + w[i]
            val t2 = (a.rotateRight(2) xor a.rotateRight(13) xor a.rotateRight(22)) + ((a and b) xor (a and c) xor (b and c))
            hh = g; g = f; f = e; e = d + t1; d = c; c = b; b = a; a = t1 + t2
        }
        h[0] += a; h[1] += b; h[2] += c; h[3] += d; h[4] += e; h[5] += f; h[6] += g; h[7] += hh
    }
    return ByteArray(32) { (h[it / 4] ushr (24 - 8 * (it % 4))).toByte() }
}

private fun ByteArray.toHex(): String = joinToString("") { (it.toInt() and 0xff).toString(16).padStart(2, '0') }

private const val base64UrlAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

internal fun base64UrlEncode(bytes: ByteArray): String = buildString {
    for (i in bytes.indices step 3) {
        val chunk = (bytes[i].toInt() and 0xff shl 16) or
            ((bytes.getOrNull(i + 1)?.toInt() ?: 0) and 0xff shl 8) or ((bytes.getOrNull(i + 2)?.toInt() ?: 0) and 0xff)
        val chars = when (bytes.size - i) { 1 -> 2; 2 -> 3; else -> 4 }
        for (j in 0 until chars) append(base64UrlAlphabet[chunk ushr (18 - 6 * j) and 0x3f])
    }
}

internal fun base64UrlDecode(value: String): ByteArray {
    require(value.length % 4 != 1) { "Invalid base64url length." }
    var buffer = 0
    var bits = 0
    val out = ArrayList<Byte>()
    for (char in value) {
        val digit = base64UrlAlphabet.indexOf(char)
        require(digit >= 0) { "Invalid base64url character." }
        buffer = (buffer shl 6) or digit
        bits += 6
        if (bits >= 8) {
            bits -= 8
            out.add((buffer ushr bits).toByte())
        }
    }
    require(buffer and ((1 shl bits) - 1) == 0) { "Non-canonical base64url." }
    return out.toByteArray()
}
