package com.gustavomartins.codexpool.data

import io.ktor.http.Url
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

object PairingLinks {
    private val mutable = MutableStateFlow<String?>(null)
    val pending = mutable.asStateFlow()
    fun receive(link: String) { mutable.value = link }
    fun clear() { mutable.value = null }
}

/** A runner QR: the pairing request and, from current runners, the key that signs its commands. */
data class PairingQr(val requestId: String, val commandKey: String?)

fun parsePairingLink(link: String, pool: String): PairingQr {
    require(link.length <= 1024) { "QR de pareamento inválido." }
    val url = Url(link)
    val names = url.parameters.names()
    require(url.protocol.name == "codexpool" && url.host == "pair" && url.encodedPath.isEmpty() &&
        url.user.isNullOrEmpty() && url.password.isNullOrEmpty() && url.fragment.isEmpty() &&
        (names == setOf("pool", "request") || names == setOf("pool", "request", "key")) &&
        names.all { url.parameters.getAll(it)?.size == 1 }) { "Use o QR gerado pelo runnerd." }
    require(url.parameters["pool"] == pool) { "O QR pertence a outro pool. Nenhuma credencial foi enviada." }
    val request = requireNotNull(url.parameters["request"]).also {
        require(it.matches(Regex("[A-Za-z0-9_-]{43}"))) { "Pedido de pareamento inválido." }
    }
    val key = url.parameters["key"]?.also { CommandSigning.decodeKey(it) }
    return PairingQr(request, key)
}

fun pairingRequestId(link: String, pool: String): String = parsePairingLink(link, pool).requestId

@Serializable data class PairingReview(
    val id: String, val name: String, val os: String, val arch: String,
    @SerialName("runner_id") val runnerId: String? = null,
    @SerialName("expires_at") val expiresAt: String,
)
@Serializable data class PairingDecision(
    val decision: Decision,
    @SerialName("user_code") val userCode: String,
)
