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

fun pairingRequestId(link: String, pool: String): String {
    require(link.length <= 1024) { "QR de pareamento inválido." }
    val url = Url(link)
    require(url.protocol.name == "codexpool" && url.host == "pair" && url.encodedPath.isEmpty() &&
        url.user.isNullOrEmpty() && url.password.isNullOrEmpty() && url.fragment.isEmpty() &&
        url.parameters.names() == setOf("pool", "request") && url.parameters.getAll("pool")?.size == 1 &&
        url.parameters.getAll("request")?.size == 1) { "Use o QR gerado pelo runnerd." }
    require(url.parameters["pool"] == pool) { "O QR pertence a outro pool. Nenhuma credencial foi enviada." }
    return requireNotNull(url.parameters["request"]).also {
        require(it.matches(Regex("[A-Za-z0-9_-]{43}"))) { "Pedido de pareamento inválido." }
    }
}

@Serializable data class PairingReview(
    val id: String, val name: String, val os: String, val arch: String,
    @SerialName("runner_id") val runnerId: String? = null,
    @SerialName("expires_at") val expiresAt: String,
)
@Serializable data class PairingDecision(
    val decision: Decision,
    @SerialName("user_code") val userCode: String,
)
