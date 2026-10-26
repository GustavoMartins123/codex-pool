package com.gustavomartins.codexpool.data

import io.ktor.client.HttpClient
import io.ktor.client.request.header
import io.ktor.client.request.request
import io.ktor.client.request.setBody
import io.ktor.client.statement.bodyAsChannel
import io.ktor.http.ContentType
import io.ktor.http.HttpMethod
import io.ktor.http.content.TextContent
import io.ktor.http.isSuccess
import io.ktor.utils.io.readRemaining
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout
import kotlinx.io.readByteArray
import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds

/**
 * Builds the client for one runner QR. Implementations pin TLS to the QR's
 * SPKI hash and accept only the QR's hosts, and share nothing with the pool
 * client: no cookies, no bearer, no proxy, no redirects, no body logging.
 * Platforms without one cannot pair runners from the LAN.
 */
fun interface BootstrapClientFactory {
    fun create(payload: BootstrapPayload): HttpClient
}

class BootstrapFailure(val code: BootstrapError?, message: String) : IllegalStateException(message)

/** The runner's temporary LAN service; the QR secret is its only credential. */
class BootstrapApi internal constructor(private val client: HttpClient, private val payload: BootstrapPayload, val endpoint: String) {
    suspend fun status(): BootstrapStatus = call(HttpMethod.Get, "/status", null)
    suspend fun begin(operationId: String, poolUrl: String): BootstrapTicket =
        call(HttpMethod.Post, "/begin", PoolJson.encodeToString(BootstrapBegin(payload.bootstrapId, operationId, poolUrl)))
    suspend fun ack(operationId: String) { call<Unit>(HttpMethod.Post, "/ack", PoolJson.encodeToString(BootstrapOperation(payload.bootstrapId, operationId))) }
    suspend fun cancel(operationId: String) { call<Unit>(HttpMethod.Post, "/cancel", PoolJson.encodeToString(BootstrapOperation(payload.bootstrapId, operationId))) }
    fun close() = client.close()

    private suspend inline fun <reified T> call(method: HttpMethod, path: String, body: String?): T {
        val response = client.request(endpoint + RunnerContract.BootstrapPathPrefix + path) {
            this.method = method
            header(RunnerContract.BootstrapSecretHeader, payload.bootstrapSecret)
            if (body != null) setBody(TextContent(body, ContentType.Application.Json))
        }
        val bytes = response.bodyAsChannel().readRemaining(RunnerContract.BootstrapMaxResponseBytes + 1L).readByteArray()
        check(bytes.size <= RunnerContract.BootstrapMaxResponseBytes) { "Resposta do computador grande demais." }
        val text = bytes.decodeToString()
        if (!response.status.isSuccess()) {
            val failure = runCatching { PoolJson.decodeFromString<BootstrapStatus>(text) }.getOrNull()
            throw BootstrapFailure(failure?.error, bootstrapMessage(failure?.error) ?: when (response.status.value) {
                401 -> "O computador recusou o QR. Leia o QR exibido agora."
                409 -> "Este QR já está sendo usado por outro pareamento."
                410 -> "O QR expirou. Leia o QR novo exibido no computador."
                else -> "O computador recusou o pedido (HTTP ${response.status.value})."
            })
        }
        @Suppress("UNCHECKED_CAST")
        return if (T::class == Unit::class) Unit as T else PoolJson.decodeFromString(text)
    }

    companion object {
        /** Tries the QR's addresses at once and keeps the first that answers with this QR's identity. */
        suspend fun connect(factory: BootstrapClientFactory, payload: BootstrapPayload, timeout: Duration = 8.seconds): BootstrapApi {
            val client = factory.create(payload)
            val winner = coroutineScope {
                val first = CompletableDeferred<BootstrapApi?>()
                val attempts = payload.endpoints.map { endpoint ->
                    launch {
                        val api = BootstrapApi(client, payload, endpoint)
                        val ok = runCatching { withTimeout(timeout) { api.status() } }.getOrNull()?.bootstrapId == payload.bootstrapId
                        if (ok) first.complete(api)
                    }
                }
                launch { attempts.joinAll(); first.complete(null) }
                first.await().also { coroutineContext[kotlinx.coroutines.Job]?.children?.forEach { it.cancel() } }
            }
            if (winner == null) {
                client.close()
                error("Não foi possível falar com o computador pela rede local. O celular precisa alcançar o computador diretamente: " +
                    "use a mesma rede, sem isolamento entre aparelhos, e libere o runnerd no firewall.")
            }
            return winner
        }
    }
}

fun bootstrapMessage(code: BootstrapError?): String? = when (code) {
    BootstrapError.PoolUnreachable -> "O computador não conseguiu acessar o pool. Confira a internet do computador."
    BootstrapError.PoolRejected -> "O pool recusou o pedido de pareamento."
    BootstrapError.PoolIncompatible -> "O pool precisa ser atualizado para parear runners pelo app."
    BootstrapError.PairingDenied -> "O pareamento foi negado ou a conta está indisponível."
    BootstrapError.PairingExpired -> "O pedido expirou. Leia o QR novo exibido no computador."
    BootstrapError.SaveFailed -> "O computador não conseguiu salvar a credencial. Reinicie o runnerd em poucos minutos para recuperá-la."
    null -> null
}
